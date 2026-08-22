//go:build unit

package migrations

import (
	"io/fs"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseSections_FileWithoutMarkersIsWholeUp(t *testing.T) {
	content := "-- plain\nCREATE TABLE IF NOT EXISTS t (id INT);\nDROP TABLE IF EXISTS legacy;"
	sections, err := ParseSections(content)
	require.NoError(t, err)
	require.False(t, sections.HasMarkers)
	require.Equal(t, content, sections.Up, "没有注解的文件必须原样执行，保持向后兼容")
	require.Empty(t, sections.Down)

	execSQL, err := ExecutableSQL(content)
	require.NoError(t, err)
	require.Equal(t, content, execSQL)
}

func TestParseSections_UpAndDownAreSeparated(t *testing.T) {
	content := `-- header comment before markers
-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS t (id INT);
CREATE INDEX IF NOT EXISTS idx_t ON t (id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS t;
-- +goose StatementEnd`
	sections, err := ParseSections(content)
	require.NoError(t, err)
	require.True(t, sections.HasMarkers)
	require.Contains(t, sections.Up, "CREATE TABLE IF NOT EXISTS t")
	require.Contains(t, sections.Up, "CREATE INDEX IF NOT EXISTS idx_t")
	require.NotContains(t, sections.Up, "DROP TABLE")
	require.NotContains(t, sections.Up, "+goose", "注解行不是 SQL，不应出现在执行文本里")
	require.Contains(t, sections.Down, "DROP TABLE IF EXISTS t")
	require.NotContains(t, sections.Down, "+goose")

	execSQL, err := ExecutableSQL(content)
	require.NoError(t, err)
	require.Equal(t, sections.Up, execSQL)
}

func TestParseSections_DirectiveSpellingIsLenient(t *testing.T) {
	content := "--+goose up\nCREATE TABLE t (id INT);\n--  +GOOSE   Down\nDROP TABLE t;"
	sections, err := ParseSections(content)
	require.NoError(t, err)
	require.True(t, sections.HasMarkers)
	require.Equal(t, "CREATE TABLE t (id INT);", sections.Up)
	require.Equal(t, "DROP TABLE t;", sections.Down)
}

func TestParseSections_RejectsMalformedFiles(t *testing.T) {
	cases := map[string]struct {
		content string
		want    error
		text    string
	}{
		"only Down": {
			content: "-- +goose Down\nDROP TABLE t;",
			want:    errGooseDownBeforeUp,
		},
		"Down before Up": {
			content: "-- +goose Down\nDROP TABLE t;\n-- +goose Up\nCREATE TABLE t (id INT);",
			want:    errGooseDownBeforeUp,
		},
		"empty Up": {
			content: "-- +goose Up\n-- nothing here\n\n-- +goose Down\nDROP TABLE t;",
			want:    errGooseEmptyUp,
		},
		"SQL before the first marker": {
			content: "CREATE TABLE early (id INT);\n-- +goose Up\nCREATE TABLE t (id INT);",
			want:    errGooseSQLBeforeUp,
		},
		"duplicate Up": {
			content: "-- +goose Up\nCREATE TABLE t (id INT);\n-- +goose Up\nCREATE TABLE u (id INT);",
			want:    errGooseDuplicateUp,
		},
		"duplicate Down": {
			content: "-- +goose Up\nCREATE TABLE t (id INT);\n-- +goose Down\nDROP TABLE t;\n-- +goose Down\nDROP TABLE t;",
			want:    errGooseDuplicateDown,
		},
		"unsupported directive": {
			content: "-- +goose NO TRANSACTION\n-- +goose Up\nCREATE INDEX CONCURRENTLY idx ON t (id);",
			text:    "unsupported goose directive \"no transaction\"",
		},
		"StatementBegin outside sections": {
			content: "-- +goose StatementBegin\nCREATE TABLE t (id INT);\n-- +goose StatementEnd",
			text:    "outside of an Up/Down section",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSections(tc.content)
			require.Error(t, err)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			}
			if tc.text != "" {
				require.Contains(t, err.Error(), tc.text)
			}
			_, err = ExecutableSQL(tc.content)
			require.Error(t, err, "ExecutableSQL 与 ParseSections 必须给出同样的判定")
		})
	}
}

// TestEmbeddedMigrations_GooseSectionsAreWellFormed 遍历嵌入的全部迁移文件，而不是点名
// 019/024/037：新加的带注解文件当天就被覆盖。
func TestEmbeddedMigrations_GooseSectionsAreWellFormed(t *testing.T) {
	names := embeddedMigrationNames(t)
	require.NotEmpty(t, names)

	markedFiles := 0
	for _, name := range names {
		content := readEmbeddedMigration(t, name)
		sections, err := ParseSections(content)
		require.NoErrorf(t, err, "%s: goose sections must parse; the runner refuses files it cannot section", name)

		execSQL, err := ExecutableSQL(content)
		require.NoError(t, err)
		for _, line := range strings.Split(execSQL, "\n") {
			_, isDirective := gooseDirective(line)
			require.Falsef(t, isDirective, "%s: directive line %q must never reach PostgreSQL", name, line)
		}
		require.NotEmptyf(t, strings.TrimSpace(StripComments(execSQL)), "%s: executable SQL must not be empty", name)

		if !sections.HasMarkers {
			require.Equalf(t, content, execSQL, "%s: files without markers run in full", name)
			continue
		}
		markedFiles++
		require.Equalf(t, sections.Up, execSQL, "%s: only the Up section is executable", name)
		for _, stmt := range SplitStatements(StripComments(sections.Down)) {
			require.NotContainsf(t, execSQL, stmt, "%s: a Down statement leaked into the executable SQL", name)
		}
	}
	require.Greater(t, markedFiles, 0, "the embedded set is known to contain goose-marked files; if they were all rewritten this guard needs a synthetic fixture")
}

func embeddedMigrationNames(t *testing.T) []string {
	t.Helper()
	names, err := fs.Glob(FS, "*.sql")
	require.NoError(t, err)
	sort.Strings(names)
	return names
}

func readEmbeddedMigration(t *testing.T, name string) string {
	t.Helper()
	data, err := FS.ReadFile(name)
	require.NoError(t, err)
	return strings.TrimSpace(string(data))
}
