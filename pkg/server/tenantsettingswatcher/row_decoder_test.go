// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package tenantsettingswatcher_test

import (
	"context"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/server/tenantsettingswatcher"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/stretchr/testify/require"
)

// TestRowDecoder simply verifies that the row decoder can safely decode the
// rows stored in the tenant_settings table of a real cluster.
func TestRowDecoder(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx := context.Background()
	srv, db, kvDB := serverutils.StartServer(t, base.TestServerArgs{})
	defer srv.Stopper().Stop(ctx)
	ts := srv.ApplicationLayer()

	tdb := sqlutils.MakeSQLRunner(db)

	toSet := map[settings.InternalKey]struct {
		tenantID int
		val      string
		typ      string
	}{
		"a.bool.setting": {
			tenantID: 0,
			val:      "true",
			typ:      "b",
		},
		"a.duration.setting": {
			tenantID: 2,
			val:      "17s",
			typ:      "d",
		},
		"a.float.setting": {
			tenantID: 2,
			val:      "0.23",
			typ:      "f",
		},
	}
	for k, v := range toSet {
		tdb.Exec(
			t, "INSERT INTO system.tenant_settings (tenant_id, name, value, value_type) VALUES ($1, $2, $3, $4)",
			v.tenantID, k, v.val, v.typ,
		)
	}

	tableID, err := ts.SystemTableIDResolver().(catalog.SystemTableIDResolver).LookupSystemTableID(ctx, "tenant_settings")
	require.NoError(t, err)
	k := ts.Codec().TablePrefix(uint32(tableID))
	rows, err := kvDB.Scan(ctx, k, k.PrefixEnd(), 0 /* maxRows */)
	require.NoError(t, err)
	dec := tenantsettingswatcher.MakeRowDecoder()
	for _, row := range rows {
		kv := roachpb.KeyValue{
			Key:   row.Key,
			Value: *row.Value,
		}

		tenantID, setting, tombstone, err := dec.DecodeRow(kv)
		require.NoError(t, err)
		require.False(t, tombstone)
		if exp, ok := toSet[setting.InternalKey]; ok {
			require.Equal(t, exp.tenantID, int(tenantID.InternalValue))
			require.Equal(t, exp.val, setting.Value.Value)
			require.Equal(t, exp.typ, setting.Value.Type)
			delete(toSet, setting.InternalKey)
		}

		// Test the tombstone logic while we're here.
		kv.Value.Reset()
		tombstoneTenantID, tombstoneSetting, tombstone, err := dec.DecodeRow(kv)
		require.NoError(t, err)
		require.True(t, tombstone)
		require.Equal(t, tenantID, tombstoneTenantID)
		require.Equal(t, setting.InternalKey, tombstoneSetting.InternalKey)
		require.Zero(t, tombstoneSetting.Value.Value)
		require.Zero(t, tombstoneSetting.Value.Type)
	}
	require.Len(t, toSet, 0)
}
