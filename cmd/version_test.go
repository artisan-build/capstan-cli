package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCommandPrintsNonEmptyVersion(t *testing.T) {
	t.Parallel()

	rootCmd := newRootCommand()
	output := new(bytes.Buffer)
	rootCmd.SetOut(output)
	rootCmd.SetArgs([]string{"version"})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("version command returned error: %v", err)
	}

	if strings.TrimSpace(output.String()) == "" {
		t.Fatal("version command printed an empty version")
	}
}
