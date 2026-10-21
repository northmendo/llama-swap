package discovery

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mostlygeek/llama-swap/internal/config"
)

func TestScan_Run_DraftsAndSkips(t *testing.T) {
	cfg, modelDir := discoveryTestConfig(t, `
  engine_rules:
    - match: "MTP"
      engine: bigbuild
`)

	// draftable: dense model
	w, count := minimalModelKVs()
	path := filepath.Join(modelDir, "Saluki-27B-Q4_K_M.gguf")
	require.NoError(t, os.WriteFile(path, buildGGUFWith(3, uint64(count), w.buf.Bytes()), 0o644))

	// draftable: MTP model, hits the engine rule
	w2 := &kvWriter{}
	w2.string("general.architecture", "qwen35")
	w2.string("general.type", "model")
	w2.uint64v("qwen35.nextn_predict_layers", 1)
	w2.uint32v("qwen35.context_length", 262144)
	w2.uint32v("qwen35.block_count", 65)
	path2 := filepath.Join(modelDir, "Underdog-Saluki-MTP.gguf")
	require.NoError(t, os.WriteFile(path2, buildGGUFWith(3, 5, w2.buf.Bytes()), 0o644))

	// skipped: adapter (LoRA)
	w3 := &kvWriter{}
	w3.string("general.architecture", "llama")
	w3.string("general.type", "adapter")
	path3 := filepath.Join(modelDir, "some-lora.gguf")
	require.NoError(t, os.WriteFile(path3, buildGGUFWith(3, 2, w3.buf.Bytes()), 0o644))

	// skipped: not a GGUF at all (>= header size so the magic is checked)
	path4 := filepath.Join(modelDir, "broken.gguf")
	require.NoError(t, os.WriteFile(path4, []byte("this is definitely not a gguf file!!"), 0o644))

	// ignored: not a .gguf file
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "notes.txt"), []byte("hi"), 0o644))

	result, err := Run(cfg)
	require.NoError(t, err)

	assert.Equal(t, 4, result.Found)
	assert.Equal(t, 2, result.Drafted)
	assert.Equal(t, 2, result.Skipped)
	assert.Equal(t, "4 found / 2 draftable / 2 skipped", result.SummaryLine())

	byPath := make(map[string]FileResult, len(result.Results))
	for _, fr := range result.Results {
		byPath[fr.Path] = fr
	}

	dense := byPath[path]
	require.Equal(t, StatusDrafted, dense.Status)
	require.NotNil(t, dense.Draft)
	assert.Equal(t, "fastbuild", dense.Draft.Engine, "no rule match falls back to first defined engine")
	assert.Contains(t, dense.EngineSource, "default: first defined engine")
	assert.Contains(t, dense.Draft.Args, "--port ${PORT}")
	assert.Contains(t, dense.Draft.Args, "--ctx-size 4096", "trained ctx below cap is used verbatim")

	mtp := byPath[path2]
	require.Equal(t, StatusDrafted, mtp.Status)
	assert.Equal(t, "bigbuild", mtp.Draft.Engine, "MTP rule must match")
	assert.Contains(t, mtp.EngineSource, "engine_rules[0]")
	assert.Equal(t, int64(32768), mtp.Draft.CtxSize, "262144 trained ctx capped at default 32768")
	assert.True(t, mtp.Draft.CtxCapped)
	require.NotNil(t, mtp.Metadata)
	assert.Equal(t, map[string]any{"qwen35.nextn_predict_layers": int64(1)}, mtp.Metadata.MTPKeys)

	adapter := byPath[path3]
	assert.Equal(t, StatusSkipped, adapter.Status)
	assert.Contains(t, adapter.SkipReason, `general.type is "adapter"`)

	broken := byPath[path4]
	assert.Equal(t, StatusSkipped, broken.Status)
	assert.Contains(t, broken.SkipReason, "not a GGUF file")
	assert.NotEmpty(t, broken.MetadataError)
}

func TestScan_Run_RecursiveAndSorted(t *testing.T) {
	cfg, modelDir := discoveryTestConfig(t, "")

	sub := filepath.Join(modelDir, "sub", "deeper")
	require.NoError(t, os.MkdirAll(sub, 0o755))

	w, count := minimalModelKVs()
	top := filepath.Join(modelDir, "b-model.gguf")
	deep := filepath.Join(sub, "a-model.gguf")
	require.NoError(t, os.WriteFile(top, buildGGUFWith(3, uint64(count), w.buf.Bytes()), 0o644))
	require.NoError(t, os.WriteFile(deep, buildGGUFWith(3, uint64(count), w.buf.Bytes()), 0o644))

	result, err := Run(cfg)
	require.NoError(t, err)
	require.Len(t, result.Results, 2)
	assert.Equal(t, top, result.Results[0].Path, "results are sorted by path")
	assert.Equal(t, deep, result.Results[1].Path)
}

func TestScan_Run_DedupesOverlappingFolders(t *testing.T) {
	cfg, modelDir := discoveryTestConfig(t, "")

	// reconfigure: the folder and its parent... instead scan the same
	// folder twice via a second discovery entry.
	cfg.Discovery.Folders = append(cfg.Discovery.Folders, modelDir)

	w, count := minimalModelKVs()
	require.NoError(t, os.WriteFile(filepath.Join(modelDir, "only.gguf"), buildGGUFWith(3, uint64(count), w.buf.Bytes()), 0o644))

	result, err := Run(cfg)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Found, "a file listed by two configured folders is found once")
}

func TestScan_Run_ConfigErrors(t *testing.T) {
	t.Run("no discovery section", func(t *testing.T) {
		_, err := Run(config.Config{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no discovery section")
	})

	t.Run("no folders", func(t *testing.T) {
		_, err := Run(config.Config{Discovery: &config.DiscoveryConfig{}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "discovery.folders is empty")
	})
}

func TestScan_Run_UnreadableFolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based test")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	require.NoError(t, os.MkdirAll(locked, 0o755))

	cfg := config.Config{Discovery: &config.DiscoveryConfig{Folders: []string{locked}}}
	_, err := Run(cfg)
	require.NoError(t, err, "a readable empty folder scans fine")

	// A fully unreadable folder aborts the scan with a clear error.
	t.Chdir(dir)
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	_, err = Run(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scanning")
}
