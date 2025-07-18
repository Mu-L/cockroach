// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package changefeedccl

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/blobs"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/cdcevent"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/changefeedbase"
	"github.com/cockroachdb/cockroach/pkg/cloud"
	"github.com/cockroachdb/cockroach/pkg/cloud/cloudpb"
	_ "github.com/cockroachdb/cockroach/pkg/cloud/impl" // register cloud storage providers
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/security/username"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/tabledesc"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/ioctx"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/cockroachdb/cockroach/pkg/util/span"
	"github.com/cockroachdb/errors"
	"github.com/klauspost/compress/gzip"
	"github.com/stretchr/testify/require"
)

const unlimitedFileSize int64 = math.MaxInt64

func makeTopic(name string) *tableDescriptorTopic {
	id, _ := strconv.ParseUint(name, 36, 64)
	desc := tabledesc.NewBuilder(&descpb.TableDescriptor{Name: name, ID: descpb.ID(id)}).BuildImmutableTable()
	spec := changefeedbase.Target{
		Type:              jobspb.ChangefeedTargetSpecification_PRIMARY_FAMILY_ONLY,
		DescID:            desc.GetID(),
		StatementTimeName: changefeedbase.StatementTimeName(name),
	}
	return &tableDescriptorTopic{Metadata: makeMetadata(desc), spec: spec}
}

func makeMetadata(desc catalog.TableDescriptor) cdcevent.Metadata {
	return cdcevent.Metadata{
		TableID:   desc.GetID(),
		TableName: desc.GetName(),
		Version:   desc.GetVersion(),
	}
}

func TestCloudStorageSink(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	ctx := context.Background()

	externalIODir, dirCleanupFn := testutils.TempDir(t)
	defer dirCleanupFn()

	gzipDecompress := func(t *testing.T, compressed []byte) []byte {
		r, err := gzip.NewReader(bytes.NewReader(compressed))
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			require.NoError(t, r.Close())
		}()

		decompressed, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		return decompressed
	}

	listLeafDirectories := func(t *testing.T) []string {
		absRoot := filepath.Join(externalIODir, testDir(t))

		var folders []string

		hasChildDirs := func(path string) bool {
			files, err := os.ReadDir(path)
			if err != nil {
				return false
			}
			for _, file := range files {
				if file.IsDir() {
					return true
				}
			}
			return false
		}

		walkDirFn := func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path == absRoot {
				return nil
			}
			if d.IsDir() && !hasChildDirs(path) {
				relPath, _ := filepath.Rel(absRoot, path)
				folders = append(folders, relPath)
			}
			return nil
		}

		require.NoError(t, filepath.WalkDir(absRoot, walkDirFn))
		return folders
	}

	// slurpDir returns the contents of every file under root (relative to the
	// temp dir created above), sorted by the name of the file.
	slurpDir := func(t *testing.T) []string {
		var files []string
		walkDirFn := func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			file, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.HasSuffix(path, ".gz") {
				file = gzipDecompress(t, file)
			}
			files = append(files, string(file))
			return nil
		}
		absRoot := filepath.Join(externalIODir, testDir(t))
		require.NoError(t, os.MkdirAll(absRoot, 0755))
		require.NoError(t, filepath.WalkDir(absRoot, walkDirFn))
		return files
	}

	var noKey []byte
	settings := cluster.MakeTestingClusterSettings()
	opts := changefeedbase.EncodingOptions{
		Format:     changefeedbase.OptFormatJSON,
		Envelope:   changefeedbase.OptEnvelopeWrapped,
		KeyInValue: true,
		// NB: compression added in single-node subtest.
	}
	ts := func(i int64) hlc.Timestamp { return hlc.Timestamp{WallTime: i} }
	e, err := makeJSONEncoder(ctx, jsonEncoderOptions{EncodingOptions: opts}, getTestingEnrichedSourceProvider(t, opts), makeChangefeedTargets("foo"))
	require.NoError(t, err)

	clientFactory := blobs.TestBlobServiceClient(externalIODir)
	externalStorageFromURI := func(ctx context.Context, uri string, user username.SQLUsername, opts ...cloud.ExternalStorageOption) (cloud.ExternalStorage,
		error) {
		var options cloud.ExternalStorageOptions
		for _, opt := range opts {
			opt(&options)
		}
		require.Equal(t, options.ClientName, "cdc")

		return cloud.ExternalStorageFromURI(ctx, uri, base.ExternalIODirConfig{}, settings,
			clientFactory,
			user,
			nil, /* db */
			nil, /* limiters */
			cloud.NilMetrics,
			opts...)
	}

	user := username.RootUserName()

	testWithAndWithoutAsyncFlushing := func(t *testing.T, name string, testFn func(*testing.T)) {
		t.Helper()
		testutils.RunTrueAndFalse(t, name+"/asyncFlush", func(t *testing.T, enable bool) {
			old := enableAsyncFlush.Get(&settings.SV)
			enableAsyncFlush.Override(context.Background(), &settings.SV, enable)
			defer enableAsyncFlush.Override(context.Background(), &settings.SV, old)
			testFn(t)
		})
	}

	testWithAndWithoutAsyncFlushing(t, `golden`, func(t *testing.T) {
		t1 := makeTopic(`t1`)
		testSpan := roachpb.Span{Key: []byte("a"), EndKey: []byte("b")}
		sf, err := span.MakeFrontier(testSpan)
		require.NoError(t, err)
		timestampOracle := &changeAggregatorLowerBoundOracle{sf: sf}

		s, err := makeCloudStorageSink(
			ctx, sinkURI(t, unlimitedFileSize), 1, settings, opts,
			timestampOracle, externalStorageFromURI, user, nil, nil,
		)
		require.NoError(t, err)
		defer func() { require.NoError(t, s.Close()) }()
		s.(*cloudStorageSink).sinkID = 7 // Force a deterministic sinkID.

		require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(`v1`), ts(1), ts(1), zeroAlloc, nil))
		require.NoError(t, s.Flush(ctx))

		require.Equal(t, []string{
			"v1\n",
		}, slurpDir(t))

		require.NoError(t, s.EmitResolvedTimestamp(ctx, e, ts(5)))
		resolvedFile, err := os.ReadFile(filepath.Join(
			externalIODir, testDir(t), `1970-01-01`, `197001010000000000000050000000000.RESOLVED`))
		require.NoError(t, err)
		require.Equal(t, `{"resolved":"5.0000000000"}`, string(resolvedFile))
	})

	forwardFrontier := func(f span.Frontier, s roachpb.Span, wall int64) bool {
		forwarded, err := f.Forward(s, ts(wall))
		require.NoError(t, err)
		return forwarded
	}

	stringOrDefault := func(s, ifEmpty string) string {
		if len(s) == 0 {
			return ifEmpty
		}
		return s
	}

	testWithAndWithoutAsyncFlushing(t, `single-node`, func(t *testing.T) {
		before := opts.Compression
		// Compression codecs include buffering that interferes with other tests,
		// e.g. the bucketing test that configures very small flush sizes.
		defer func() {
			opts.Compression = before
		}()
		for _, compression := range []string{"", "gzip"} {
			opts.Compression = compression
			t.Run("compress="+stringOrDefault(compression, "none"), func(t *testing.T) {
				t1 := makeTopic(`t1`)
				t2 := makeTopic(`t2`)

				testSpan := roachpb.Span{Key: []byte("a"), EndKey: []byte("b")}
				sf, err := span.MakeFrontier(testSpan)
				require.NoError(t, err)
				timestampOracle := &changeAggregatorLowerBoundOracle{sf: sf}
				s, err := makeCloudStorageSink(
					ctx, sinkURI(t, unlimitedFileSize), 1, settings, opts,
					timestampOracle, externalStorageFromURI, user, nil, nil,
				)
				require.NoError(t, err)
				defer func() { require.NoError(t, s.Close()) }()
				s.(*cloudStorageSink).sinkID = 7 // Force a deterministic sinkID.

				// Empty flush emits no files.
				require.NoError(t, s.Flush(ctx))
				require.Equal(t, []string(nil), slurpDir(t))

				// Emitting rows and flushing should write them out in one file per table. Note
				// the ordering among these two files is non-deterministic as either of them could
				// be flushed first (and thus be assigned fileID 0).
				var pool testAllocPool
				require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(`v1`), ts(1), ts(1), pool.alloc(), nil))
				require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(`v2`), ts(1), ts(1), pool.alloc(), nil))
				require.NoError(t, s.EmitRow(ctx, t2, noKey, []byte(`w1`), ts(3), ts(3), pool.alloc(), nil))
				require.NoError(t, s.Flush(ctx))
				require.EqualValues(t, 0, pool.used())
				expected := []string{
					"v1\nv2\n",
					"w1\n",
				}
				actual := slurpDir(t)
				sort.Strings(actual)
				require.Equal(t, expected, actual)

				// Flushing with no new emits writes nothing new.
				require.NoError(t, s.Flush(ctx))
				actual = slurpDir(t)
				sort.Strings(actual)
				require.Equal(t, expected, actual)

				// Without a flush, nothing new shows up.
				require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(`v3`), ts(3), ts(3), zeroAlloc, nil))
				actual = slurpDir(t)
				sort.Strings(actual)
				require.Equal(t, expected, actual)

				// Note that since we haven't forwarded `testSpan` yet, all files initiated until
				// this point must have the same `frontier` timestamp. Since fileID increases
				// monotonically, the last file emitted should be ordered as such.
				require.NoError(t, s.Flush(ctx))
				require.Equal(t, []string{
					"v3\n",
				}, slurpDir(t)[2:])

				// Data from different versions of a table is put in different files, so that we
				// can guarantee that all rows in any given file have the same schema.
				// We also advance `testSpan` and `Flush` to make sure these new rows are read
				// after the rows emitted above.
				require.True(t, forwardFrontier(sf, testSpan, 4))
				require.NoError(t, s.Flush(ctx))
				require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(`v4`), ts(4), ts(4), zeroAlloc, nil))
				t1.Version = 2
				require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(`v5`), ts(5), ts(5), zeroAlloc, nil))
				require.NoError(t, s.Flush(ctx))
				expected = []string{
					"v4\n",
					"v5\n",
				}
				actual = slurpDir(t)
				actual = actual[len(actual)-2:]
				sort.Strings(actual)
				require.Equal(t, expected, actual)
			})
		}
	})

	testWithAndWithoutAsyncFlushing(t, `multi-node`, func(t *testing.T) {
		t1 := makeTopic(`t1`)
		testSpan := roachpb.Span{Key: []byte("a"), EndKey: []byte("b")}
		sf, err := span.MakeFrontier(testSpan)
		require.NoError(t, err)
		timestampOracle := &changeAggregatorLowerBoundOracle{sf: sf}
		s1, err := makeCloudStorageSink(
			ctx, sinkURI(t, unlimitedFileSize), 1, settings, opts,
			timestampOracle, externalStorageFromURI, user, nil, nil,
		)
		require.NoError(t, err)
		defer func() { require.NoError(t, s1.Close()) }()
		s2, err := makeCloudStorageSink(
			ctx, sinkURI(t, unlimitedFileSize), 2, settings, opts,
			timestampOracle, externalStorageFromURI, user, nil, nil,
		)
		defer func() { require.NoError(t, s2.Close()) }()
		require.NoError(t, err)
		// Hack into the sinks to pretend each is the first sink created on two
		// different nodes, which is the worst case for them conflicting.
		s1.(*cloudStorageSink).sinkID = 0
		s2.(*cloudStorageSink).sinkID = 0

		// Force deterministic job session IDs to force ordering of output files.
		s1.(*cloudStorageSink).jobSessionID = "a"
		s2.(*cloudStorageSink).jobSessionID = "b"

		// Each node writes some data at the same timestamp. When this data is
		// written out, the files have different names and don't conflict because
		// the sinks have different job session IDs.
		require.NoError(t, s1.EmitRow(ctx, t1, noKey, []byte(`v1`), ts(1), ts(1), zeroAlloc, nil))
		require.NoError(t, s2.EmitRow(ctx, t1, noKey, []byte(`w1`), ts(1), ts(1), zeroAlloc, nil))
		require.NoError(t, s1.Flush(ctx))
		require.NoError(t, s2.Flush(ctx))
		require.Equal(t, []string{
			"v1\n",
			"w1\n",
		}, slurpDir(t))

		// If a node restarts then the entire distsql flow has to restart. If
		// this happens before checkpointing, some data is written again but
		// this is unavoidable.
		s1R, err := makeCloudStorageSink(
			ctx, sinkURI(t, unbuffered), 1, settings, opts,
			timestampOracle, externalStorageFromURI, user, nil, nil,
		)
		require.NoError(t, err)
		defer func() { require.NoError(t, s1R.Close()) }()
		s2R, err := makeCloudStorageSink(
			ctx, sinkURI(t, unbuffered), 2, settings, opts,
			timestampOracle, externalStorageFromURI, user, nil, nil,
		)
		require.NoError(t, err)
		defer func() { require.NoError(t, s2R.Close()) }()
		// Nodes restart. s1 gets the same sink id it had last time but s2
		// doesn't.
		s1R.(*cloudStorageSink).sinkID = 0
		s2R.(*cloudStorageSink).sinkID = 7

		// Again, force deterministic job session IDs to force ordering of output
		// files. Note that making s1R have the same job session ID as s1 should make
		// its output overwrite s1's output.
		s1R.(*cloudStorageSink).jobSessionID = "a"
		s2R.(*cloudStorageSink).jobSessionID = "b"
		// Each resends the data it did before.
		require.NoError(t, s1R.EmitRow(ctx, t1, noKey, []byte(`v1`), ts(1), ts(1), zeroAlloc, nil))
		require.NoError(t, s2R.EmitRow(ctx, t1, noKey, []byte(`w1`), ts(1), ts(1), zeroAlloc, nil))
		require.NoError(t, s1R.Flush(ctx))
		require.NoError(t, s2R.Flush(ctx))
		// s1 data ends up being overwritten, s2 data ends up duplicated.
		require.Equal(t, []string{
			"v1\n",
			"w1\n",
			"w1\n",
		}, slurpDir(t))
	})

	// The jobs system can't always clean up perfectly after itself and so there
	// are situations where it will leave a zombie job coordinator for a bit.
	// Make sure the zombie isn't writing the same filenames so that it can't
	// overwrite good data with partial data.
	//
	// This test is also sufficient for verifying the behavior of a multi-node
	// changefeed using this sink. Ditto job restarts.
	testWithAndWithoutAsyncFlushing(t, `zombie`, func(t *testing.T) {
		t1 := makeTopic(`t1`)
		testSpan := roachpb.Span{Key: []byte("a"), EndKey: []byte("b")}
		sf, err := span.MakeFrontier(testSpan)
		require.NoError(t, err)
		timestampOracle := &changeAggregatorLowerBoundOracle{sf: sf}
		s1, err := makeCloudStorageSink(
			ctx, sinkURI(t, unlimitedFileSize), 1, settings, opts,
			timestampOracle, externalStorageFromURI, user, nil, nil,
		)
		require.NoError(t, err)
		defer func() { require.NoError(t, s1.Close()) }()
		s1.(*cloudStorageSink).sinkID = 7         // Force a deterministic sinkID.
		s1.(*cloudStorageSink).jobSessionID = "a" // Force deterministic job session ID.
		s2, err := makeCloudStorageSink(
			ctx, sinkURI(t, unlimitedFileSize), 1, settings, opts,
			timestampOracle, externalStorageFromURI, user, nil, nil,
		)
		require.NoError(t, err)
		defer func() { require.NoError(t, s2.Close()) }()
		s2.(*cloudStorageSink).sinkID = 8         // Force a deterministic sinkID.
		s2.(*cloudStorageSink).jobSessionID = "b" // Force deterministic job session ID.

		// Good job writes
		require.NoError(t, s1.EmitRow(ctx, t1, noKey, []byte(`v1`), ts(1), ts(1), zeroAlloc, nil))
		require.NoError(t, s1.EmitRow(ctx, t1, noKey, []byte(`v2`), ts(2), ts(2), zeroAlloc, nil))
		require.NoError(t, s1.Flush(ctx))

		// Zombie job writes partial duplicate data
		require.NoError(t, s2.EmitRow(ctx, t1, noKey, []byte(`v1`), ts(1), ts(1), zeroAlloc, nil))
		require.NoError(t, s2.Flush(ctx))

		// Good job continues. There are duplicates in the data but nothing was
		// lost.
		require.NoError(t, s1.EmitRow(ctx, t1, noKey, []byte(`v3`), ts(3), ts(3), zeroAlloc, nil))
		require.NoError(t, s1.Flush(ctx))
		require.Equal(t, []string{
			"v1\nv2\n",
			"v3\n",
			"v1\n",
		}, slurpDir(t))
	})

	waitAsyncFlush := func(s Sink) error {
		return s.(*cloudStorageSink).waitAsyncFlush(context.Background())
	}
	testWithAndWithoutAsyncFlushing(t, `bucketing`, func(t *testing.T) {
		t1 := makeTopic(`t1`)
		testSpan := roachpb.Span{Key: []byte("a"), EndKey: []byte("b")}
		sf, err := span.MakeFrontier(testSpan)
		require.NoError(t, err)
		timestampOracle := &changeAggregatorLowerBoundOracle{sf: sf}
		const targetMaxFileSize = 6
		s, err := makeCloudStorageSink(
			ctx, sinkURI(t, targetMaxFileSize), 1, settings, opts,
			timestampOracle, externalStorageFromURI, user, nil, nil,
		)
		require.NoError(t, err)
		defer func() { require.NoError(t, s.Close()) }()
		s.(*cloudStorageSink).sinkID = 7 // Force a deterministic sinkID.

		// Writing more than the max file size chunks the file up and flushes it
		// out as necessary.
		for i := int64(1); i <= 5; i++ {
			require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(fmt.Sprintf(`v%d`, i)), ts(i), ts(i), zeroAlloc, nil))
		}
		require.NoError(t, waitAsyncFlush(s))
		require.Equal(t, []string{
			"v1\nv2\nv3\n",
		}, slurpDir(t))

		// Flush then writes the rest.
		require.NoError(t, s.Flush(ctx))
		require.Equal(t, []string{
			"v1\nv2\nv3\n",
			"v4\nv5\n",
		}, slurpDir(t))

		// Forward the SpanFrontier here and trigger an empty flush to update
		// the sink's `inclusiveLowerBoundTs`
		_, err = sf.Forward(testSpan, ts(5))
		require.NoError(t, err)
		require.NoError(t, s.Flush(ctx))

		// Some more data is written. Some of it flushed out because of the max
		// file size.
		for i := int64(6); i < 10; i++ {
			require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(fmt.Sprintf(`v%d`, i)), ts(i), ts(i), zeroAlloc, nil))
		}
		require.NoError(t, waitAsyncFlush(s))
		require.Equal(t, []string{
			"v1\nv2\nv3\n",
			"v4\nv5\n",
			"v6\nv7\nv8\n",
		}, slurpDir(t))

		// Resolved timestamps are periodically written. This happens
		// asynchronously from a different node and can be given an earlier
		// timestamp than what's been handed to EmitRow, but the system
		// guarantees that Flush been called (and returned without error) with a
		// ts at >= this one before this call starts.
		//
		// The resolved timestamp file should precede the data files that were
		// started after the SpanFrontier was forwarded to ts(5).
		require.NoError(t, s.EmitResolvedTimestamp(ctx, e, ts(5)))
		require.Equal(t, []string{
			"v1\nv2\nv3\n",
			"v4\nv5\n",
			`{"resolved":"5.0000000000"}`,
			"v6\nv7\nv8\n",
		}, slurpDir(t))

		// Flush then writes the rest. Since we use the time of the EmitRow
		// or EmitResolvedTimestamp calls to order files, the resolved timestamp
		// file should precede the last couple files since they started buffering
		// after the SpanFrontier was forwarded to ts(5).
		require.NoError(t, s.Flush(ctx))
		require.Equal(t, []string{
			"v1\nv2\nv3\n",
			"v4\nv5\n",
			`{"resolved":"5.0000000000"}`,
			"v6\nv7\nv8\n",
			"v9\n",
		}, slurpDir(t))

		// A resolved timestamp emitted with ts > 5 should follow everything
		// emitted thus far.
		require.NoError(t, s.EmitResolvedTimestamp(ctx, e, ts(6)))
		require.Equal(t, []string{
			"v1\nv2\nv3\n",
			"v4\nv5\n",
			`{"resolved":"5.0000000000"}`,
			"v6\nv7\nv8\n",
			"v9\n",
			`{"resolved":"6.0000000000"}`,
		}, slurpDir(t))
	})

	testWithAndWithoutAsyncFlushing(t, `partition-formatting`, func(t *testing.T) {
		t1 := makeTopic(`t1`)
		testSpan := roachpb.Span{Key: []byte("a"), EndKey: []byte("b")}
		const targetMaxFileSize = 6

		opts := opts

		timestamps := []time.Time{
			time.Date(2000, time.January, 1, 1, 1, 1, 0, time.UTC),
			time.Date(2000, time.January, 1, 1, 2, 1, 0, time.UTC),
			time.Date(2000, time.January, 1, 2, 1, 1, 0, time.UTC),
			time.Date(2000, time.January, 2, 1, 1, 1, 0, time.UTC),
			time.Date(2000, time.January, 2, 6, 1, 1, 0, time.UTC),
		}

		for _, tc := range []struct {
			format          string
			expectedFolders []string
		}{
			{
				"hourly",
				[]string{
					"2000-01-01/01",
					"2000-01-01/02",
					"2000-01-02/01",
					"2000-01-02/06",
				},
			},
			{
				"daily",
				[]string{
					"2000-01-01",
					"2000-01-02",
				},
			},
			{
				"flat",
				[]string{},
			},
			{
				"", // should fall back to default
				[]string{
					"2000-01-01",
					"2000-01-02",
				},
			},
		} {
			testWithAndWithoutAsyncFlushing(t, stringOrDefault(tc.format, "default"),
				func(t *testing.T) {
					sf, err := span.MakeFrontier(testSpan)
					require.NoError(t, err)
					timestampOracle := &changeAggregatorLowerBoundOracle{sf: sf}

					sinkURIWithParam := sinkURI(t, targetMaxFileSize)
					sinkURIWithParam.AddParam(changefeedbase.SinkParamPartitionFormat, tc.format)
					t.Logf("format=%s sinkgWithParam: %s", tc.format, sinkURIWithParam.String())
					s, err := makeCloudStorageSink(
						ctx, sinkURIWithParam, 1, settings, opts,
						timestampOracle, externalStorageFromURI, user, nil, nil,
					)

					require.NoError(t, err)
					defer func() { require.NoError(t, s.Close()) }()
					s.(*cloudStorageSink).sinkID = 7 // Force a deterministic sinkID.

					for i, timestamp := range timestamps {
						hlcTime := ts(timestamp.UnixNano())

						// Move the frontier and flush to update the dataFilePartition value
						_, err = sf.Forward(testSpan, hlcTime)
						require.NoError(t, err)
						require.NoError(t, s.Flush(ctx))

						require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(fmt.Sprintf(`v%d`, i)), hlcTime, hlcTime, zeroAlloc, nil))
					}

					require.NoError(t, s.Flush(ctx)) // Flush the last file
					require.ElementsMatch(t, tc.expectedFolders, listLeafDirectories(t))
					require.Equal(t, []string{"v0\n", "v1\n", "v2\n", "v3\n", "v4\n"}, slurpDir(t))
				})
		}
	})

	testWithAndWithoutAsyncFlushing(t, `file-ordering`, func(t *testing.T) {
		t1 := makeTopic(`t1`)
		testSpan := roachpb.Span{Key: []byte("a"), EndKey: []byte("b")}
		sf, err := span.MakeFrontier(testSpan)
		require.NoError(t, err)
		timestampOracle := &changeAggregatorLowerBoundOracle{sf: sf}
		s, err := makeCloudStorageSink(
			ctx, sinkURI(t, unlimitedFileSize), 1, settings, opts,
			timestampOracle, externalStorageFromURI, user, nil, nil,
		)
		require.NoError(t, err)
		defer func() { require.NoError(t, s.Close()) }()
		s.(*cloudStorageSink).sinkID = 7 // Force a deterministic sinkID.

		// Simulate initial scan, which emits data at a timestamp, then an equal
		// resolved timestamp.
		require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(`is1`), ts(1), ts(1), zeroAlloc, nil))
		require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(`is2`), ts(1), ts(1), zeroAlloc, nil))
		require.NoError(t, s.Flush(ctx))
		require.NoError(t, s.EmitResolvedTimestamp(ctx, e, ts(1)))

		// Test some edge cases.

		// Forward the testSpan and trigger an empty `Flush` to have new rows
		// be after the resolved timestamp emitted above.
		require.True(t, forwardFrontier(sf, testSpan, 2))
		require.NoError(t, s.Flush(ctx))
		require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(`e2`), ts(2), ts(2), zeroAlloc, nil))
		require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(`e3prev`), ts(3).Prev(), ts(3).Prev(), zeroAlloc, nil))
		require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(`e3`), ts(3), ts(3), zeroAlloc, nil))
		require.True(t, forwardFrontier(sf, testSpan, 3))
		require.NoError(t, s.Flush(ctx))
		require.NoError(t, s.EmitResolvedTimestamp(ctx, e, ts(3)))
		require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(`e3next`), ts(3).Next(), ts(3).Next(), zeroAlloc, nil))
		require.NoError(t, s.Flush(ctx))
		require.NoError(t, s.EmitResolvedTimestamp(ctx, e, ts(4)))

		require.Equal(t, []string{
			"is1\nis2\n",
			`{"resolved":"1.0000000000"}`,
			"e2\ne3prev\ne3\n",
			`{"resolved":"3.0000000000"}`,
			"e3next\n",
			`{"resolved":"4.0000000000"}`,
		}, slurpDir(t))

		// Test that files with timestamp lower than the least resolved timestamp
		// as of file creation time are ignored.
		require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(`noemit`), ts(1).Next(), ts(1).Next(), zeroAlloc, nil))
		require.Equal(t, []string{
			"is1\nis2\n",
			`{"resolved":"1.0000000000"}`,
			"e2\ne3prev\ne3\n",
			`{"resolved":"3.0000000000"}`,
			"e3next\n",
			`{"resolved":"4.0000000000"}`,
		}, slurpDir(t))
	})

	testWithAndWithoutAsyncFlushing(t, `ordering-among-schema-versions`, func(t *testing.T) {
		t1 := makeTopic(`t1`)
		testSpan := roachpb.Span{Key: []byte("a"), EndKey: []byte("b")}
		sf, err := span.MakeFrontier(testSpan)
		require.NoError(t, err)
		timestampOracle := &changeAggregatorLowerBoundOracle{sf: sf}
		var targetMaxFileSize int64 = 10
		s, err := makeCloudStorageSink(
			ctx, sinkURI(t, targetMaxFileSize), 1, settings, opts,
			timestampOracle, externalStorageFromURI, user, nil, nil)
		require.NoError(t, err)
		defer func() { require.NoError(t, s.Close()) }()

		require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(`v1`), ts(1), ts(1), zeroAlloc, nil))
		t1.Version = 1
		require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(`v3`), ts(1), ts(1), zeroAlloc, nil))
		// Make the first file exceed its file size threshold. This should trigger a flush
		// for the first file but not the second one.
		t1.Version = 0
		require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(`trigger-flush-v1`), ts(1), ts(1), zeroAlloc, nil))
		require.NoError(t, waitAsyncFlush(s))
		require.Equal(t, []string{
			"v1\ntrigger-flush-v1\n",
		}, slurpDir(t))

		// Now make the file with the newer schema exceed its file size threshold and ensure
		// that the file with the older schema is flushed (and ordered) before.
		require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(`v2`), ts(1), ts(1), zeroAlloc, nil))
		t1.Version = 1
		require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(`trigger-flush-v3`), ts(1), ts(1), zeroAlloc, nil))
		require.NoError(t, waitAsyncFlush(s))
		require.Equal(t, []string{
			"v1\ntrigger-flush-v1\n",
			"v2\n",
			"v3\ntrigger-flush-v3\n",
		}, slurpDir(t))

		// Calling `Flush()` on the sink should emit files in the order of their schema IDs.
		require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(`w1`), ts(1), ts(1), zeroAlloc, nil))
		t1.Version = 0
		require.NoError(t, s.EmitRow(ctx, t1, noKey, []byte(`x1`), ts(1), ts(1), zeroAlloc, nil))
		require.NoError(t, s.Flush(ctx))
		require.Equal(t, []string{
			"v1\ntrigger-flush-v1\n",
			"v2\n",
			"v3\ntrigger-flush-v3\n",
			"x1\n",
			"w1\n",
		}, slurpDir(t))
	})

	// Verify no goroutines leaked when using compression.
	testWithAndWithoutAsyncFlushing(t, `no goroutine leaks with compression`, func(t *testing.T) {
		before := opts.Compression
		// Compression codecs include buffering that interferes with other tests,
		// e.g. the bucketing test that configures very small flush sizes.
		defer func() {
			opts.Compression = before
		}()

		topic := makeTopic(`t1`)

		for _, compression := range []string{"gzip", "zstd"} {
			opts.Compression = compression
			t.Run("compress="+stringOrDefault(compression, "none"), func(t *testing.T) {
				timestampOracle := explicitTimestampOracle(ts(1))
				s, err := makeCloudStorageSink(
					ctx, sinkURI(t, unlimitedFileSize), 1, settings, opts,
					timestampOracle, externalStorageFromURI, user, nil, nil,
				)
				require.NoError(t, err)

				rng, _ := randutil.NewPseudoRand()
				data := randutil.RandBytes(rng, 1024)
				// Write few megs worth of data.
				for n := 0; n < 20; n++ {
					eventTS := ts(int64(n + 1))
					require.NoError(t, s.EmitRow(ctx, topic, noKey, data, eventTS, eventTS, zeroAlloc, nil))
				}

				// Close the sink.  That's it -- we rely on leaktest detector to determine
				// if the underlying compressor leaked go routines.
				require.NoError(t, s.Close())
			})
		}
	})

	// Verify no goroutines leaked when using compression with context cancellation.
	testWithAndWithoutAsyncFlushing(t, `no goroutine leaks when context canceled`, func(t *testing.T) {
		before := opts.Compression
		// Compression codecs include buffering that interferes with other tests,
		// e.g. the bucketing test that configures very small flush sizes.
		defer func() {
			opts.Compression = before
		}()

		topic := makeTopic(`t1`)

		for _, compression := range []string{"gzip", "zstd"} {
			opts.Compression = compression
			t.Run("compress="+stringOrDefault(compression, "none"), func(t *testing.T) {
				timestampOracle := explicitTimestampOracle(ts(1))
				s, err := makeCloudStorageSink(
					ctx, sinkURI(t, unlimitedFileSize), 1, settings, opts,
					timestampOracle, externalStorageFromURI, user, nil, nil,
				)
				require.NoError(t, err)
				defer func() {
					require.NoError(t, s.Close())
				}()

				// We need to run the following code inside separate
				// closure so that we capture the set of goroutines started
				// while writing the data (and ignore goroutines started by the sink
				// itself).
				func() {
					defer leaktest.AfterTest(t)()

					rng, _ := randutil.NewPseudoRand()
					data := randutil.RandBytes(rng, 1024)
					// Write few megs worth of data.
					for n := 0; n < 20; n++ {
						eventTS := ts(int64(n + 1))
						require.NoError(t, s.EmitRow(ctx, topic, noKey, data, eventTS, eventTS, zeroAlloc, nil))
					}
					cancledCtx, cancel := context.WithCancel(ctx)
					cancel()

					// Write 1 more piece of data.  We want to make sure that when error happens
					// (context cancellation in this case) that any resources used by compression
					// codec are released (this is checked by leaktest).
					require.Equal(t, context.Canceled, s.EmitRow(cancledCtx, topic, noKey, data, ts(1), ts(1), zeroAlloc, nil))
				}()
			})
		}
	})
}

type explicitTimestampOracle hlc.Timestamp

func (o explicitTimestampOracle) inclusiveLowerBoundTS() hlc.Timestamp {
	return hlc.Timestamp(o)
}

// TestCloudStorageSinkFastGzip is a regression test for #129947.
// The original issue was a memory leak from pgzip, the library used for fast
// gzip compression for cloud storage. The leak was caused by a race condition
// between Flush and the async flusher: if the Flush clears files before the
// async flusher closes the compression codec as part of flushing the files,
// and the flush returns an error, the compression codec will not be closed
// properly. This test uses some test-only synchronization points in the cloud
// storage sink to test for the regression.
func TestCloudStorageSinkFastGzip(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	settings := cluster.MakeTestingClusterSettings()

	useFastGzip.Override(context.Background(), &settings.SV, true)
	enableAsyncFlush.Override(context.Background(), &settings.SV, true)

	opts := changefeedbase.EncodingOptions{
		Format:      changefeedbase.OptFormatJSON,
		Envelope:    changefeedbase.OptEnvelopeWrapped,
		KeyInValue:  true,
		Compression: "gzip",
	}

	testSpan := roachpb.Span{Key: []byte("a"), EndKey: []byte("b")}
	sf, err := span.MakeFrontier(testSpan)
	require.NoError(t, err)
	timestampOracle := &changeAggregatorLowerBoundOracle{sf: sf}

	// Force the storage sink to always return an error.
	getErrorWriter := func() io.WriteCloser {
		return errorWriter{}
	}
	mockStorageSink := func(_ context.Context, _ string, _ username.SQLUsername, _ ...cloud.ExternalStorageOption) (cloud.ExternalStorage, error) {
		return &mockSinkStorage{writer: getErrorWriter}, nil
	}

	// The cloud storage sink calls the AsyncFlushSync function in two different
	// goroutines: once in Flush(), and once in the async flusher. By waiting for
	// the two goroutines to both reach those points, we can trigger the original
	// issue, which was caused by a race condition between the two goroutines
	// leading to leaked compression library resources.
	wg := sync.WaitGroup{}
	waiter := func() {
		wg.Done()
		wg.Wait()
	}
	testingKnobs := &TestingKnobs{AsyncFlushSync: waiter}
	const sizeInBytes = 100 * 1024 * 1024 // 100MB

	// Test that there's no leak during an async Flush.
	t.Run("flush", func(t *testing.T) {
		wg.Add(2)
		s, err := makeCloudStorageSink(
			ctx, sinkURI(t, unlimitedFileSize), 1, settings, opts, timestampOracle,
			mockStorageSink, username.RootUserName(), nil /* mb */, testingKnobs,
		)
		require.NoError(t, err)
		s.(*cloudStorageSink).sinkID = 7 // Force a deterministic sinkID.

		var noKey []byte
		for i := 1; i < 10; i++ {
			newTopic := makeTopic(fmt.Sprintf(`t%d`, i))
			byteSlice := make([]byte, sizeInBytes)
			ts := hlc.Timestamp{WallTime: int64(i)}
			_ = s.EmitRow(ctx, newTopic, noKey, byteSlice, ts, ts, zeroAlloc, nil)
		}

		// Flush the files and close the sink. Any leaks should be caught after the
		// test by leaktest.
		_ = s.Flush(ctx)
		_ = s.Close()
	})
	// Test that there's no leak during an async flushTopicVersions.
	t.Run("flushTopicVersions", func(t *testing.T) {
		wg.Add(2)
		s, err := makeCloudStorageSink(
			ctx, sinkURI(t, 2*sizeInBytes), 1, settings, opts, timestampOracle,
			mockStorageSink, username.RootUserName(), nil /* mb */, testingKnobs,
		)
		require.NoError(t, err)
		s.(*cloudStorageSink).sinkID = 7 // Force a deterministic sinkID.

		// Insert data to the same topic with different versions so that they are
		// in different files.
		var noKey []byte
		newTopic := makeTopic("test")
		for i := 1; i < 10; i++ {
			byteSlice := make([]byte, sizeInBytes)
			ts := hlc.Timestamp{WallTime: int64(i)}
			newTopic.Version++
			_ = s.EmitRow(ctx, newTopic, noKey, byteSlice, ts, ts, zeroAlloc, nil)
		}

		// Flush the files and close the sink. Any leaks should be caught after the
		// test by leaktest.
		_ = s.(*cloudStorageSink).flushTopicVersions(ctx, newTopic.GetTableName(), int64(newTopic.GetVersion()))
		_ = s.Close()
	})
}

func testDir(t *testing.T) string {
	return strings.ReplaceAll(t.Name(), "/", ";")
}

func sinkURI(t *testing.T, maxFileSize int64) *changefeedbase.SinkURL {
	u, err := url.Parse(fmt.Sprintf("nodelocal://1/%s", testDir(t)))
	require.NoError(t, err)
	sink := &changefeedbase.SinkURL{URL: u}
	if maxFileSize != unlimitedFileSize {
		sink.AddParam(changefeedbase.SinkParamFileSize, strconv.FormatInt(maxFileSize, 10))
	}
	return sink
}

// errorWriter always returns an error on writes.
type errorWriter struct{}

func (errorWriter) Write(_ []byte) (int, error) {
	return 0, errors.New("write error")
}
func (errorWriter) Close() error { return nil }

// mockSinkStorage can be useful for testing to override the WriteCloser.
type mockSinkStorage struct {
	writer func() io.WriteCloser
}

var _ cloud.ExternalStorage = &mockSinkStorage{}

func (n *mockSinkStorage) Close() error {
	return nil
}

func (n *mockSinkStorage) Conf() cloudpb.ExternalStorage {
	return cloudpb.ExternalStorage{Provider: cloudpb.ExternalStorageProvider_null}
}

func (n *mockSinkStorage) ExternalIOConf() base.ExternalIODirConfig {
	return base.ExternalIODirConfig{}
}

func (n *mockSinkStorage) RequiresExternalIOAccounting() bool {
	return false
}

func (n *mockSinkStorage) Settings() *cluster.Settings {
	return nil
}

func (n *mockSinkStorage) ReadFile(
	_ context.Context, _ string, _ cloud.ReadOptions,
) (ioctx.ReadCloserCtx, int64, error) {
	return nil, 0, io.EOF
}

func (n *mockSinkStorage) Writer(_ context.Context, _ string) (io.WriteCloser, error) {
	return n.writer(), nil
}

func (n *mockSinkStorage) List(_ context.Context, _, _ string, _ cloud.ListingFn) error {
	return nil
}

func (n *mockSinkStorage) Delete(_ context.Context, _ string) error {
	return nil
}

func (n *mockSinkStorage) Size(_ context.Context, _ string) (int64, error) {
	return 0, nil
}
