package webmail

import (
	"strings"
	"testing"
	"testing/fstest"
)

const testManifest = `{
  "src/main.ts": {"file": "assets/main-abc.js", "isEntry": true,
                  "css": ["assets/main-abc.css"], "imports": ["_shared-def.js"],
                  "dynamicImports": ["src/islands/compose.tsx"]},
  "_shared-def.js": {"file": "assets/shared-def.js", "css": ["assets/shared-def.css"]},
  "src/islands/compose.tsx": {"file": "assets/compose-ghi.js", "isDynamicEntry": true}
}`

func TestAssetTags(t *testing.T) {
	a := loadAssets(fstest.MapFS{"dist/manifest.json": {Data: []byte(testManifest)}})
	got := string(a.tags("src/main.ts"))

	for _, want := range []string{
		`<link rel="stylesheet" href="/static/dist/assets/main-abc.css">`,
		`<link rel="stylesheet" href="/static/dist/assets/shared-def.css">`,
		`<script type="module" src="/static/dist/assets/main-abc.js"></script>`,
		`<link rel="modulepreload" href="/static/dist/assets/shared-def.js">`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("tags missing %q in:\n%s", want, got)
		}
	}
	// Dynamic imports load on demand and must not be preloaded.
	if strings.Contains(got, "compose-ghi") {
		t.Errorf("dynamic import should not be preloaded:\n%s", got)
	}
}

func TestAssetTagsDevMode(t *testing.T) {
	a := loadAssets(fstest.MapFS{})
	a.dev.Store(true)
	got := string(a.tags("src/main.ts"))
	if !strings.Contains(got, `src="/@vite/client"`) || !strings.Contains(got, `src="/src/main.ts"`) {
		t.Errorf("dev tags = %q", got)
	}
}

func TestAssetTagsMissingManifest(t *testing.T) {
	a := loadAssets(fstest.MapFS{})
	got := string(a.tags("src/main.ts"))
	if !strings.HasPrefix(got, "<!-- webmail assets not built") {
		t.Errorf("want explanatory comment, got %q", got)
	}
	if a.loadErr == nil {
		t.Error("loadErr should record the missing manifest")
	}
}

func TestAssetTagsUnknownEntry(t *testing.T) {
	a := loadAssets(fstest.MapFS{"dist/manifest.json": {Data: []byte(testManifest)}})
	if got := string(a.tags("src/nope.ts")); !strings.Contains(got, "not in manifest") {
		t.Errorf("got %q", got)
	}
}
