package config

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every setting the loader reads must appear in configs/dev.env. That file is what the
// README sends people to, so a key documented nowhere is a key nobody can discover: it was
// exactly how CG_NATS_STREAM stayed invisible while mattering to anyone running two
// deployments against one NATS cluster. Prose drifts silently; this does not.
func TestEveryKeyIsDocumented(t *testing.T) {
	source, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	env, err := os.ReadFile("../../configs/dev.env")
	if err != nil {
		t.Fatalf("read dev.env: %v", err)
	}

	reads := regexp.MustCompile(`l\.(?:str|boolVal|intVal|floatVal|duration)\("([A-Z0-9_]+)"`)
	matches := reads.FindAllStringSubmatch(string(source), -1)
	if len(matches) == 0 {
		t.Fatal("found no settings at all — the pattern stopped matching the loader")
	}

	var undocumented []string
	for _, m := range matches {
		key := "CG_" + m[1]
		if !strings.Contains(string(env), key) {
			undocumented = append(undocumented, key)
		}
	}
	sort.Strings(undocumented)
	if len(undocumented) > 0 {
		t.Errorf("settings absent from configs/dev.env: %s\n"+
			"Document each with what it is for and why the default is what it is.",
			strings.Join(undocumented, ", "))
	}
}
