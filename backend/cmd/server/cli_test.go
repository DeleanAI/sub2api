package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func withIsolatedSubcommands(t *testing.T) {
	t.Helper()
	saved := subcommands
	subcommands = map[string]subcommand{}
	t.Cleanup(func() { subcommands = saved })
}

func TestRunSubcommand_UnknownAndFlagsFallThrough(t *testing.T) {
	withIsolatedSubcommands(t)
	var out, errOut bytes.Buffer

	handled, err := runSubcommand(nil, &out, &errOut)
	require.False(t, handled)
	require.NoError(t, err)

	handled, err = runSubcommand([]string{"-version"}, &out, &errOut)
	require.False(t, handled, "legacy flags must keep going through flag.Parse")
	require.NoError(t, err)

	handled, err = runSubcommand([]string{"does-not-exist"}, &out, &errOut)
	require.False(t, handled)
	require.NoError(t, err)
}

func TestRunSubcommand_DispatchesAndPropagatesError(t *testing.T) {
	withIsolatedSubcommands(t)
	var gotArgs []string
	boom := errors.New("boom")
	registerSubcommand(subcommand{
		name:    "probe",
		summary: "test command",
		run: func(args []string, stdout, stderr io.Writer) error {
			gotArgs = args
			return boom
		},
	})

	var out, errOut bytes.Buffer
	handled, err := runSubcommand([]string{"probe", "a", "b"}, &out, &errOut)
	require.True(t, handled)
	require.ErrorIs(t, err, boom)
	require.Equal(t, []string{"a", "b"}, gotArgs)
}

func TestRunSubcommand_HelpListsEveryRegisteredCommand(t *testing.T) {
	withIsolatedSubcommands(t)
	for _, name := range []string{"zeta", "alpha"} {
		registerSubcommand(subcommand{name: name, summary: name + " summary", run: func([]string, io.Writer, io.Writer) error { return nil }})
	}
	var out, errOut bytes.Buffer
	handled, err := runSubcommand([]string{"help"}, &out, &errOut)
	require.True(t, handled)
	require.NoError(t, err)
	text := out.String()
	// 遍历注册表断言，而不是点名：新增子命令当天就被覆盖。
	for name, cmd := range subcommands {
		require.Contains(t, text, name)
		require.Contains(t, text, cmd.summary)
	}
	require.Less(t, strings.Index(text, "alpha"), strings.Index(text, "zeta"), "help output must be sorted")
}

func TestRegisterSubcommand_RejectsDuplicatesAndFlagLikeNames(t *testing.T) {
	withIsolatedSubcommands(t)
	noop := func([]string, io.Writer, io.Writer) error { return nil }
	registerSubcommand(subcommand{name: "x", run: noop})
	require.Panics(t, func() { registerSubcommand(subcommand{name: "x", run: noop}) })
	require.Panics(t, func() { registerSubcommand(subcommand{name: "-x", run: noop}) })
	require.Panics(t, func() { registerSubcommand(subcommand{name: "", run: noop}) })
	require.Panics(t, func() { registerSubcommand(subcommand{name: "y"}) })
}
