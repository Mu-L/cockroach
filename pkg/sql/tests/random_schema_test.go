// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package tests_test

import (
	"context"
	gosql "database/sql"
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/spanconfig"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/randgen"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
)

func setDb(t *testing.T, db *gosql.DB, name string) {
	if _, err := db.Exec("USE " + name); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRandomSchema(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	s, db, _ := serverutils.StartServer(t, base.TestServerArgs{
		Knobs: base.TestingKnobs{
			SpanConfig: &spanconfig.TestingKnobs{
				LimiterLimitOverride: func() int64 {
					return math.MaxInt64
				},
			},
		},
	})
	defer s.Stopper().Stop(ctx)

	if _, err := db.Exec("CREATE DATABASE test; CREATE DATABASE test2"); err != nil {
		t.Fatal(err)
	}

	toStr := func(c tree.Statement) string {
		return tree.AsStringWithFlags(c, tree.FmtParsable)
	}

	rng := rand.New(rand.NewSource(timeutil.Now().UnixNano()))
	for i := 0; i < 100; i++ {
		createTable := randgen.RandCreateTable(rng, "table", i, false /* isMultiRegion */)
		setDb(t, db, "test")
		_, err := db.Exec(toStr(createTable))
		if err != nil {
			t.Fatal(createTable, err)
		}

		var quotedTabName, tabStmt, secondTabStmt string
		if err := db.QueryRow(fmt.Sprintf("SHOW CREATE TABLE %s",
			createTable.Table.String())).Scan(&quotedTabName, &tabStmt); err != nil {
			t.Fatal(err)
		}

		if quotedTabName != createTable.Table.String() {
			t.Fatalf("found table name %s, expected %s", quotedTabName, createTable.Table.String())
		}

		// Reparse the show create table statement that's stored in the database.
		parsed, err := parser.ParseOne(tabStmt)
		if err != nil {
			t.Fatalf("error parsing show create table: %s", err)
		}

		setDb(t, db, "test2")
		// Now run the SHOW CREATE TABLE statement we found on a new db and verify
		// that both tables are the same.

		_, err = db.Exec(tree.AsStringWithFlags(parsed.AST, tree.FmtParsable))
		if err != nil {
			t.Fatal(parsed.AST, err)
		}

		if err := db.QueryRow(fmt.Sprintf("SHOW CREATE TABLE %s",
			quotedTabName)).Scan(&quotedTabName, &secondTabStmt); err != nil {
			t.Fatal(err)
		}

		if quotedTabName != createTable.Table.String() {
			t.Fatalf("found table name %s, expected %s", quotedTabName, createTable.Table.String())
		}
		// Reparse the show create table statement that's stored in the database.
		secondParsed, err := parser.ParseOne(secondTabStmt)
		if err != nil {
			t.Fatalf("error parsing show create table: %s", err)
		}
		if toStr(parsed.AST) != toStr(secondParsed.AST) {
			t.Fatalf("for input statement\n%s\nfound first output\n%q\nbut second output\n%q",
				toStr(createTable), toStr(parsed.AST), toStr(secondParsed.AST))
		}
		if tabStmt != secondTabStmt {
			t.Fatalf("for input statement\n%s\nfound first output\n%q\nbut second output\n%q",
				toStr(createTable), tabStmt, secondTabStmt)
		}
	}
}
