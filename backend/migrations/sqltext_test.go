//go:build unit

package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeSQLText_StripsCommentsButKeepsStrings(t *testing.T) {
	in := "SELECT 1; -- DROP TABLE in a line comment\n/* DROP TABLE in /* nested */ block */ UPDATE t SET v = '-- not a comment' WHERE k = E'it\\'s';"
	out := normalizeSQLText(in, sqlTextOptions{})
	require.NotContains(t, out, "line comment")
	require.NotContains(t, out, "block")
	require.NotContains(t, out, "nested")
	require.Contains(t, out, "'-- not a comment'")
	require.Contains(t, out, `E'it\'s'`)
	require.Contains(t, out, "UPDATE t SET v")
}

func TestNormalizeSQLText_DollarQuotes(t *testing.T) {
	t.Run("DO block bodies execute and are kept", func(t *testing.T) {
		in := "DO $$ BEGIN -- inner comment\n ALTER TABLE t DROP COLUMN c; END $$;"
		out := normalizeSQLText(in, sqlTextOptions{blankDefinitionBodies: true})
		require.Contains(t, out, "ALTER TABLE t DROP COLUMN c")
		require.NotContains(t, out, "inner comment")
	})

	t.Run("function bodies are definitions and get blanked", func(t *testing.T) {
		in := "CREATE OR REPLACE FUNCTION f() RETURNS TRIGGER LANGUAGE plpgsql AS $body$ BEGIN UPDATE t SET v = 1; END $body$;"
		out := normalizeSQLText(in, sqlTextOptions{blankDefinitionBodies: true})
		require.NotContains(t, out, "UPDATE t SET")
		require.Contains(t, out, "CREATE OR REPLACE FUNCTION f()")
		require.Contains(t, out, "$body$", "定界符保留，便于定位")

		kept := normalizeSQLText(in, sqlTextOptions{})
		require.Contains(t, kept, "UPDATE t SET", "不要求置空时函数体原样保留")
	})

	t.Run("positional parameters are not dollar quotes", func(t *testing.T) {
		in := "SELECT $1, $2 FROM t; UPDATE t SET v = $1;"
		out := normalizeSQLText(in, sqlTextOptions{blankDefinitionBodies: true})
		require.Equal(t, in, out)
	})

	t.Run("unterminated dollar quote keeps the remainder", func(t *testing.T) {
		in := "DO $$ BEGIN DROP TABLE t;"
		out := normalizeSQLText(in, sqlTextOptions{blankDefinitionBodies: true})
		require.Contains(t, out, "DROP TABLE t")
	})
}

func TestSplitSQLStatements_DropsBlankFragments(t *testing.T) {
	parts := SplitStatements("a;\n\n;  b ;")
	require.Equal(t, []string{"a", "b"}, parts)
}

func TestNormalizeIdentifier(t *testing.T) {
	require.Equal(t, "users", normalizeIdentifier(`"Users"`))
	require.Equal(t, "users", normalizeIdentifier("public.users,"))
	require.Equal(t, "my_fn", normalizeIdentifier(" PUBLIC.my_fn) "))
	require.Equal(t, "", normalizeIdentifier("   "))
	require.Equal(t, "", strings.TrimSpace(normalizeIdentifier(";")))
}
