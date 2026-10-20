package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexTOMLEditsRespectLogicalValueBoundaries(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"literal multiline", "'''\ncli_auth_credentials_store = \"keyring\"\n[model_providers.duckway-openai]\n'''"},
		{"basic multiline", "\"\"\"\ncli_auth_credentials_store = \\\"keyring\\\"\n[model_providers.duckway-openai]\n\"\"\""},
		{"escaped triple delimiter", "\"\"\"\n\\\"\"\"\n[model_providers.duckway-openai]\n\"\"\""},
		{"literal five quote close", "'''\n[model_providers.duckway-openai]\n'''''"},
		{"nested array and inline table", "[\n{ text = '''\ncli_auth_credentials_store = \"keyring\"\n[model_providers.duckway-openai]\n''', more = [\"# [other]\", { v = \"x\" }] },\n]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := "developer_instructions = " + tc.value + "\n"
			suffix := "[projects.\"/repo#path\"] # preserved\ntrust_level = \"trusted\"\n"
			input := prefix + "cli_auth_credentials_store = 'keyring'\nmodel_provider = \"duckway-openai\"\n[model_providers.duckway-openai] # owned\nname = \"old\"\n" + suffix
			got := replaceTopLevelTOMLString(input, "cli_auth_credentials_store", "file")
			got = removeTopLevelTOMLStringValue(got, "model_provider", "duckway-openai")
			got = removeTOMLSection(got, "model_providers.duckway-openai")
			want := prefix + "cli_auth_credentials_store = \"file\"\n" + suffix
			if got != want {
				t.Fatalf("edit changed unrelated value or missed real root key:\ngot:  %q\nwant: %q", got, want)
			}
			withoutStore := prefix + suffix
			got = replaceTopLevelTOMLString(withoutStore, "cli_auth_credentials_store", "file")
			if got != want {
				t.Fatalf("store inserted inside a value: %q", got)
			}
			got = replaceTOMLSection(input, "model_providers.duckway-openai", "[model_providers.duckway-openai]\nname = \"new\"\n")
			if !strings.HasPrefix(got, prefix) || !strings.HasSuffix(got, suffix) || !strings.Contains(got, "name = \"new\"\n") || strings.Contains(got, "name = \"old\"") {
				t.Fatalf("section replacement crossed a value boundary: %q", got)
			}
		})
	}
}

func TestCodexNativeConfigReplacesCompleteMultilineStoreValue(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	prefix := "developer_instructions = '''\ncli_auth_credentials_store = \"keyring\"\n[model_providers.duckway-openai]\n'''\n"
	input := prefix + "cli_auth_credentials_store = '''\nkeyring''' # old\n[projects.\"/repo\"]\ntrust_level = \"trusted\"\n"
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockCodexConfig()
	if err != nil {
		t.Fatal(err)
	}
	err = syncCodexNativeOAuthConfig()
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := prefix + "cli_auth_credentials_store = \"file\"\n[projects.\"/repo\"]\ntrust_level = \"trusted\"\n"
	if string(data) != want {
		t.Fatalf("native store publication damaged TOML: %q", data)
	}
}
