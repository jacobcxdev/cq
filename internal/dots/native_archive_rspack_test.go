package dots

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRspackArchiveSelectedBoundedSemanticChunks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.asar")
	writeNativeArchiveFixture(t, path, map[string]string{
		"123.abc.js": "export{r as __webpack_require__};const __rspack_esm=true;",
		"456.def.js": "const state={authenticatedAccountId:null};",
		"789.abc.js": "unrelated renderer code",
	})
	modules, err := readNativeModules(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(modules) != 2 || modules["456.def.js"] == "" || modules["789.abc.js"] != "" {
		t.Fatalf("unexpected discovery: %v", modules)
	}
}

func TestRspackArchiveRejectedUnsafeEntries(t *testing.T) {
	for _, variant := range []string{"unpacked", "link", "oversized", "negative offset", "outside archive", "linked async"} {
		t.Run(variant, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "archive")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if _, err := file.WriteString("authenticatedAccountId"); err != nil {
				t.Fatal(err)
			}
			entry := nativeArchiveEntry{Size: 22, Offset: "0"}
			assets := map[string]nativeArchiveEntry{}
			switch variant {
			case "unpacked":
				entry.Unpacked = true
			case "link":
				entry.Link = "elsewhere"
			case "oversized":
				entry.Size = 33 << 20
			case "negative offset":
				entry.Offset = "-1"
			case "outside archive":
				entry.Offset = "100"
			case "linked async":
				assets["async"] = nativeArchiveEntry{Link: "elsewhere"}
			}
			assets["123.abc.js"] = entry
			if _, err := readNativeRspackModules(file, assets, 0, 22); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}

func TestRspackArchiveReadNestedAsyncChunks(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "archive")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	source := "const executor=appServices.tppLocalExecutor"
	if _, err := file.WriteString(source); err != nil {
		t.Fatal(err)
	}
	assets := map[string]nativeArchiveEntry{"async": {Files: map[string]nativeArchiveEntry{"123.abc.js": {Offset: "0", Size: int64(len(source))}}}}
	modules, err := readNativeRspackModules(file, assets, 0, int64(len(source)))
	if err != nil || modules["async/123.abc.js"] != source {
		t.Fatalf("nested discovery: %v %v", modules, err)
	}
	// These paths become import URLs; traversal and arbitrary code must fail.
	for _, path := range []string{"../123.abc.js", "async/../123.abc.js", "123.abc.js');alert(1)", "http://other/123.abc.js"} {
		if rspackAssetPattern.MatchString(path) || modulePattern.MatchString(path) {
			t.Fatalf("unsafe path accepted: %s", path)
		}
	}
}
