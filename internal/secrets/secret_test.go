package secrets

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const plain = "super-secret-value"

func TestSecretNeverPrintsPlaintext(t *testing.T) {
	s := New(plain)

	outputs := map[string]string{
		"%s":  fmt.Sprintf("%s", s),
		"%v":  fmt.Sprintf("%v", s),
		"%+v": fmt.Sprintf("%+v", s),
		"%#v": fmt.Sprintf("%#v", s),
		"%q":  fmt.Sprintf("%q", s),
		"%x":  fmt.Sprintf("%x", s),
	}
	for verb, out := range outputs {
		if strings.Contains(out, plain) {
			t.Errorf("verb %s leaked plaintext: %q", verb, out)
		}
	}
}

func TestSecretInsideStructDoesNotLeak(t *testing.T) {
	type holder struct {
		Name string
		Key  Secret
	}
	h := holder{Name: "a", Key: New(plain)}

	if out := fmt.Sprintf("%+v", h); strings.Contains(out, plain) {
		t.Errorf("struct formatting leaked: %q", out)
	}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), plain) {
		t.Errorf("json leaked: %s", b)
	}
}

func TestSecretSlogRedacts(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("config loaded", "db", New(plain))

	if strings.Contains(buf.String(), plain) {
		t.Errorf("slog leaked: %s", buf.String())
	}
	if !strings.Contains(buf.String(), redacted) {
		t.Errorf("expected redaction marker in %s", buf.String())
	}
}

func TestRevealAndEmpty(t *testing.T) {
	if got := New(plain).Reveal(); got != plain {
		t.Errorf("Reveal() = %q", got)
	}
	if !New("").IsEmpty() || New(plain).IsEmpty() {
		t.Error("IsEmpty wrong")
	}
}
