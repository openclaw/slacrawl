package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestQueryReadOnlyCommentsAndQuotedIdentifiers(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	for _, tc := range []struct{ name, query, column string }{
		{"line-comment", "-- report; comment\nselect 1 as result", "result"},
		{"line-comment-cr", "-- report\r still a comment; '\nselect 1 as result", "result"},
		{"trailing-comment-cr", "select 1 as result -- report\r ; still a comment\n", "result"},
		{"block-comment", "/* report; comment */ select 1 as result", "result"},
		{"bracket", "select 1 as [semi;colon]", "semi;colon"},
		{"backtick", "select 1 as `semi;colon`", "semi;colon"},
		{"escaped-backtick", "select 1 as `tick``;colon`", "tick`;colon"},
		{"quoted-comment-markers", "select 1 as [semi;--'/*column]", "semi;--'/*column"},
		{"double-quote-control", `select 1 as "semi;colon"`, "semi;colon"},
		{"dollar-identifier", "select 1 as a$b", "a$b"},
		{"cte", "/* report */ with [c;te] as (select 1 as result) select result from [c;te]", "result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := s.QueryReadOnly(context.Background(), tc.query)
			require.NoError(t, err)
			require.Equal(t, []map[string]any{{tc.column: int64(1)}}, rows)
			_, err = s.QueryReadOnly(context.Background(), tc.query+"; select 2 as another")
			require.ErrorContains(t, err, "single read-only select")
		})
	}
}

func TestSQLParameterStatementBoundaries(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	for _, prefix := range []string{"$", ":", "@"} {
		query := "select " + prefix + "p::suffix(a;--'/*)"
		var value int64
		require.NoError(t, s.DB().QueryRowContext(context.Background(), query, sql.Named("p::suffix(a;--'/*)", int64(1))).Scan(&value))
		require.Equal(t, int64(1), value)
		require.NoError(t, validateReadOnlyQuery(query))
		require.ErrorContains(t, validateReadOnlyQuery(query+"; select 2"), "single read-only select")
	}
}

func TestQueryReadOnlyRejectsHiddenAdditionalStatements(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"carriage-return-comment", "select 1 -- comment\r '\n; pragma query_only=off; create table escaped_guard(value)"},
		{"quoted-comment", "select 1 as [semi;--'/*column]; pragma query_only=off; create table escaped_guard(value)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			_, queryErr := s.QueryReadOnly(context.Background(), tc.query)
			rows, err := s.QueryReadOnly(context.Background(), "select name from sqlite_master where name='escaped_guard'")
			require.NoError(t, err)
			t.Logf("query error: %v; created tables: %v", queryErr, rows)
			require.ErrorContains(t, queryErr, "single read-only select")
			require.Empty(t, rows)
		})
	}
}

func TestQueryReadOnlyRejectsDuplicateResultColumns(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	for _, query := range []string{
		"select 1 as value, 2 as value",
		"select 1 as value, 2 as value where false",
	} {
		rows, err := s.QueryReadOnly(context.Background(), query)
		require.ErrorContains(t, err, "duplicate")
		require.Nil(t, rows)
	}
	rows, err := s.QueryReadOnly(context.Background(), "select 1 as value, 2 as Value")
	require.NoError(t, err)
	require.Equal(t, []map[string]any{{"value": int64(1), "Value": int64(2)}}, rows)
}

func TestQueryReadOnlyJSONValues(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	for _, tc := range []struct{ query, want string }{
		{`select json_object('channel', 'C1', 'messages', json_array('hello', 'world')) as payload`, `{"channel":"C1","messages":["hello","world"]}`},
		{`select json_group_object('channel', 'C1') as payload`, `{"channel":"C1"}`},
		{`select json_group_array(json_object('channel', 'C1')) as payload`, `[{"channel":"C1"}]`},
	} {
		rows, err := s.QueryReadOnly(context.Background(), tc.query)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		payload, ok := rows[0]["payload"].(string)
		require.True(t, ok, "JSON query result must remain text, got %T", rows[0]["payload"])
		require.JSONEq(t, tc.want, payload)
	}
}

func FuzzQueryReadOnlyQuotedAliases(f *testing.F) {
	for _, label := range []string{"", "plain", "semi;colon", "tick`inside", "-- /* '\"", "\r'--[", "名字;項目"} {
		f.Add(label)
	}
	s, err := Open(filepath.Join(f.TempDir(), "archive.db"))
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() {
		if err := s.Close(); err != nil {
			f.Error(err)
		}
	})
	f.Fuzz(func(t *testing.T, label string) {
		if len(label) > 256 || !utf8.ValidString(label) || strings.ContainsRune(label, 0) {
			t.Skip()
		}
		query := "select 1 as `" + strings.ReplaceAll(label, "`", "``") + "`"
		rows, err := s.QueryReadOnly(context.Background(), query)
		require.NoError(t, err)
		require.Equal(t, []map[string]any{{label: int64(1)}}, rows)
		_, err = s.QueryReadOnly(context.Background(), query+"; select 2")
		require.ErrorContains(t, err, "single read-only select")
		commented := "select 1 as value -- " + strings.ReplaceAll(label, "\n", " ") + "\n"
		rows, err = s.QueryReadOnly(context.Background(), commented)
		require.NoError(t, err)
		require.Equal(t, []map[string]any{{"value": int64(1)}}, rows)
		_, err = s.QueryReadOnly(context.Background(), commented+"; select 2")
		require.ErrorContains(t, err, "single read-only select")
	})
}
