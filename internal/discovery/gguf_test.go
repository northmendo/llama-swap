package discovery

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// kvWriter accumulates a GGUF KV section in memory so tests can build
// minimal fixtures as byte arrays, never as binary files on disk.
type kvWriter struct {
	buf bytes.Buffer
}

func (w *kvWriter) writeLE(size int, v uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	w.buf.Write(b[:size])
}

func (w *kvWriter) writeString(s string) {
	w.writeLE(8, uint64(len(s)))
	w.buf.WriteString(s)
}

func (w *kvWriter) writeUint32(v uint32) { w.writeLE(4, uint64(v)) }

// kv writes one typed KV entry.
func (w *kvWriter) kv(key string, typeCode int, encode func()) {
	w.writeString(key)
	w.writeUint32(uint32(typeCode))
	encode()
}

func (w *kvWriter) string(key, value string) {
	w.kv(key, ggufKVTypeString, func() { w.writeString(value) })
}

func (w *kvWriter) uint32v(key string, value uint32) {
	w.kv(key, ggufKVTypeUint32, func() { w.writeLE(4, uint64(value)) })
}

func (w *kvWriter) uint64v(key string, value uint64) {
	w.kv(key, ggufKVTypeUint64, func() { w.writeLE(8, value) })
}

func (w *kvWriter) int64v(key string, value int64) {
	w.kv(key, ggufKVTypeInt64, func() { w.writeLE(8, uint64(value)) })
}

func (w *kvWriter) float32v(key string, value float32) {
	w.kv(key, ggufKVTypeFloat32, func() { w.writeLE(4, uint64(math.Float32bits(value))) })
}

func (w *kvWriter) float64v(key string, value float64) {
	w.kv(key, ggufKVTypeFloat64, func() { w.writeLE(8, math.Float64bits(value)) })
}

func (w *kvWriter) boolv(key string, value bool) {
	w.kv(key, ggufKVTypeBool, func() {
		if value {
			w.buf.WriteByte(1)
		} else {
			w.buf.WriteByte(0)
		}
	})
}

func (w *kvWriter) uint16v(key string, value uint16) {
	w.kv(key, ggufKVTypeUint16, func() { w.writeLE(2, uint64(value)) })
}

func (w *kvWriter) int8v(key string, value int8) {
	w.kv(key, ggufKVTypeInt8, func() { w.buf.WriteByte(byte(value)) })
}

// buildGGUFWith assembles a complete minimal GGUF: header, the given KV
// section, and nothing else (the reader never touches tensor data).
func buildGGUFWith(version uint32, count uint64, kvSection []byte) []byte {
	var out bytes.Buffer
	out.WriteString("GGUF")
	var b [8]byte
	binary.LittleEndian.PutUint32(b[:4], version)
	out.Write(b[:4])
	binary.LittleEndian.PutUint64(b[:], 1) // tensor count: 1
	out.Write(b[:])
	binary.LittleEndian.PutUint64(b[:], count)
	out.Write(b[:])
	out.Write(kvSection)
	return out.Bytes()
}

// writeFixture writes bytes to a temp .gguf file and returns its path.
func writeFixture(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, content, 0o644))
	return path
}

// minimalModelKVs builds a dense llama fixture KV section.
func minimalModelKVs() (*kvWriter, int) {
	w := &kvWriter{}
	w.string("general.architecture", "llama")
	w.string("general.type", "model")
	w.uint32v("llama.context_length", 4096)
	w.uint32v("llama.block_count", 12)
	w.uint32v("llama.attention.head_count", 16)
	w.uint32v("llama.attention.head_count_kv", 4)
	return w, 6
}

func TestGGUF_ReadMetadata_Versions(t *testing.T) {
	for _, version := range []uint32{2, 3} {
		w, count := minimalModelKVs()
		path := writeFixture(t, t.TempDir(), "model-Q4_K_M.gguf", buildGGUFWith(version, uint64(count), w.buf.Bytes()))

		meta, err := ReadMetadata(path)
		require.NoError(t, err)
		assert.Equal(t, version, meta.GGUFVersion)
		assert.Equal(t, "llama", meta.Architecture)
		assert.Equal(t, "model", meta.Type)
		assert.Equal(t, int64(4096), meta.ContextLength)
		assert.Equal(t, int64(12), meta.BlockCount)
		assert.Equal(t, int64(16), meta.HeadCount)
		assert.Equal(t, int64(4), meta.HeadCountKV)
		assert.False(t, meta.MoE)
		assert.Equal(t, "Q4_K_M", meta.QuantHint)
	}
}

func TestGGUF_ReadMetadata_MoE(t *testing.T) {
	w := &kvWriter{}
	w.string("general.architecture", "bailingmoe")
	w.string("general.type", "model")
	w.uint32v("bailingmoe.context_length", 131072)
	w.uint32v("bailingmoe.block_count", 24)
	w.uint32v("bailingmoe.expert_count", 128)
	w.uint32v("bailingmoe.expert_used_count", 8)
	w.uint32v("bailingmoe.attention.head_count", 16)
	path := writeFixture(t, t.TempDir(), "moe-model-Q4_K_M.gguf", buildGGUFWith(3, 7, w.buf.Bytes()))

	meta, err := ReadMetadata(path)
	require.NoError(t, err)
	assert.Equal(t, int64(128), meta.ExpertCount)
	assert.Equal(t, int64(8), meta.ExpertUsedCount)
	assert.True(t, meta.MoE)
}

func TestGGUF_ReadMetadata_MTPObservationKeys(t *testing.T) {
	w := &kvWriter{}
	w.string("general.architecture", "qwen35")
	w.string("general.type", "model")
	w.uint64v("qwen35.nextn_predict_layers", 1)
	w.uint32v("qwen35.context_length", 262144)
	w.uint32v("qwen35.block_count", 65)
	path := writeFixture(t, t.TempDir(), "mtp-model-IQ2_S.gguf", buildGGUFWith(3, 5, w.buf.Bytes()))

	meta, err := ReadMetadata(path)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"qwen35.nextn_predict_layers": int64(1)}, meta.MTPKeys,
		"nextn_predict_layers is the MTP key real Qwen3-Next-style conversions emit")
}

func TestGGUF_ReadMetadata_MTPKeyVariants(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"llama.cpp draft model counter", "draft.llama.count"},
		{"deepseek dense split", "deepseek2.n_dense_layers"},
		{"deepseek dense layer count", "deepseek2.dense_layer_count"},
		{"mtp namespace", "mtp.layers"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &kvWriter{}
			w.string("general.architecture", "llama")
			w.uint32v(tt.key, 1)
			path := writeFixture(t, t.TempDir(), "x.gguf", buildGGUFWith(3, 2, w.buf.Bytes()))

			meta, err := ReadMetadata(path)
			require.NoError(t, err)
			assert.Contains(t, meta.MTPKeys, tt.key)
			assert.NotContains(t, joinKeys(meta.ExtraKeys), tt.key)
		})
	}
}

func joinKeys(keys []string) string { return strings.Join(keys, "\x00") }

func TestGGUF_ParseHeaderAndKVs_ValueTypes(t *testing.T) {
	w := &kvWriter{}
	w.kv("t.uint8", ggufKVTypeUint8, func() { w.buf.WriteByte(200) })
	w.int8v("t.int8", -3)
	w.uint16v("t.uint16", 65500)
	w.kv("t.int16", ggufKVTypeInt16, func() { w.writeLE(2, uint64(0xFFF3)) })
	w.uint32v("t.uint32", 4000000000)
	w.kv("t.int32", ggufKVTypeInt32, func() { w.writeLE(4, uint64(0xFFFFFFCD)) })
	w.float32v("t.float32", 1.5)
	w.boolv("t.bool", true)
	w.string("t.string", "hello")
	w.uint64v("t.uint64", 9000000000)
	w.int64v("t.int64", -9000000000)
	w.float64v("t.float64", 2.25)

	kvs, version, err := parseHeaderAndKVs(bytes.NewReader(buildGGUFWith(3, 12, w.buf.Bytes())))
	require.NoError(t, err)
	assert.Equal(t, uint32(3), version)
	assert.Equal(t, int64(200), kvs["t.uint8"])
	assert.Equal(t, int64(-3), kvs["t.int8"])
	assert.Equal(t, int64(65500), kvs["t.uint16"])
	assert.Equal(t, int64(-13), kvs["t.int16"])
	assert.Equal(t, int64(4000000000), kvs["t.uint32"])
	assert.Equal(t, int64(-51), kvs["t.int32"])
	assert.InEpsilon(t, 1.5, kvs["t.float32"].(float64), 1e-6)
	assert.Equal(t, true, kvs["t.bool"])
	assert.Equal(t, "hello", kvs["t.string"])
	assert.Equal(t, int64(9000000000), kvs["t.uint64"])
	assert.Equal(t, int64(-9000000000), kvs["t.int64"])
	assert.InEpsilon(t, 2.25, kvs["t.float64"].(float64), 1e-9)
}

func TestGGUF_ParseHeaderAndKVs_Array(t *testing.T) {
	w := &kvWriter{}
	w.kv("t.array", ggufKVTypeArray, func() {
		w.writeUint32(uint32(ggufKVTypeUint32))
		w.writeLE(8, 3)
		for _, v := range []uint32{7, 8, 9} {
			w.writeLE(4, uint64(v))
		}
	})

	kvs, _, err := parseHeaderAndKVs(bytes.NewReader(buildGGUFWith(2, 1, w.buf.Bytes())))
	require.NoError(t, err)
	assert.Equal(t, []any{int64(7), int64(8), int64(9)}, kvs["t.array"])
}

func TestGGUF_ReadMetadata_Errors(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
		wantMsg string
	}{
		{
			name:    "not a gguf",
			content: []byte("JUNKJUNKJUNKJUNKJUNKJUNK"),
			wantMsg: "not a GGUF file",
		},
		{
			name:    "truncated header",
			content: []byte("GGUF\x03\x00\x00"),
			wantMsg: "reading header",
		},
		{
			name: "version 1 rejected",
			content: func() []byte {
				b := []byte("GGUF")
				b = append(b, 1, 0, 0, 0)
				b = append(b, 1, 0, 0, 0, 0, 0, 0, 0)
				b = append(b, 0, 0, 0, 0, 0, 0, 0, 0)
				return b
			}(),
			wantMsg: "unsupported GGUF version 1",
		},
		{
			name: "zero tensors",
			content: func() []byte {
				b := []byte("GGUF")
				b = append(b, 3, 0, 0, 0)
				b = append(b, 0, 0, 0, 0, 0, 0, 0, 0)
				b = append(b, 0, 0, 0, 0, 0, 0, 0, 0)
				return b
			}(),
			wantMsg: "0 tensors",
		},
		{
			name: "truncated kv section",
			content: func() []byte {
				w := &kvWriter{}
				w.writeString("some.key") // no type or value
				return buildGGUFWith(3, 1, w.buf.Bytes())
			}(),
			wantMsg: "reading metadata type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFixture(t, t.TempDir(), "broken.gguf", tt.content)
			_, err := ReadMetadata(path)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantMsg)
		})
	}

	t.Run("missing file", func(t *testing.T) {
		_, err := ReadMetadata(filepath.Join(t.TempDir(), "nope.gguf"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "nope.gguf")
	})
}

func TestGGUF_QuantHintFromFilename(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"Saluki-27B-Q4_K_M.gguf", "Q4_K_M"},
		{"model-iq3_s.gguf", "IQ3_S"},
		{"Ternary-Bonsai-2-27B-PQ2_0.gguf", "PQ2_0"},
		{"Underdog-Saluki-27B-1.0-IQ2-mix.gguf", "IQ2-MIX"},
		{"mmproj-Qwen3.8-27B-BF16.gguf", "BF16"},
		{"embed-F16.gguf", "F16"},
		{"vocab-F32.gguf", "F32"},
		{"TQ1_0-tiny.gguf", "TQ1_0"},
		// a Q in the model name alone is not a quant tag
		{"Qwen3-4B-Instruct.gguf", ""},
		{"plain-model.gguf", ""},
		// last tag wins (quant is at the end of real filenames)
		{"Q6_K-partial-and-Q4_K_M.gguf", "Q4_K_M"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, QuantHintFromFilename(tt.name))
		})
	}
}
