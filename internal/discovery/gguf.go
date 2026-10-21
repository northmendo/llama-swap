// Package discovery implements `llama-swap discover`: it scans configured
// folders for .gguf files, extracts GGUF metadata with a pure-stdlib reader,
// and emits draft model stanzas (engine choice + args) that pass the
// existing config validation. It is tooling only — it never writes the
// config file and never starts a server.
package discovery

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
)

// GGUF file format constants. See the ggml docs:
// https://github.com/ggml-org/ggml/blob/master/docs/gguf.md
const (
	ggufMagic          = "GGUF"
	ggufMaxVersion     = 3
	ggufKVTypeUint8    = 0
	ggufKVTypeInt8     = 1
	ggufKVTypeUint16   = 2
	ggufKVTypeInt16    = 3
	ggufKVTypeUint32   = 4
	ggufKVTypeInt32    = 5
	ggufKVTypeFloat32  = 6
	ggufKVTypeBool     = 7
	ggufKVTypeString   = 8
	ggufKVTypeArray    = 9
	ggufKVTypeUint64   = 10
	ggufKVTypeInt64    = 11
	ggufKVTypeFloat64  = 12
	ggufArrayMaxLength = 1 << 20 // guards corrupted count fields
)

// GGUFError reports a parse failure with file-relative context.
type GGUFError struct {
	Path   string
	Detail string
	Err    error
}

func (e *GGUFError) Error() string {
	msg := fmt.Sprintf("parse %s: %s", e.Path, e.Detail)
	if e.Err != nil {
		return msg + ": " + e.Err.Error()
	}
	return msg
}

func (e *GGUFError) Unwrap() error { return e.Err }

// Metadata is the distilled GGUF key/value metadata a draft is built from.
// Numeric KV values are normalized to int64 (ints) or float64 (floats),
// whatever width they were stored at.
type Metadata struct {
	// Path and FileSize describe the scanned file itself.
	Path     string `json:"path"`
	FileSize int64  `json:"fileSize"`

	// GGUFVersion is the version field of the header (2 or 3).
	GGUFVersion uint32 `json:"ggufVersion"`

	// Architecture is general.architecture, e.g. "llama", "qwen3".
	Architecture string `json:"architecture"`

	// Type is general.type when present: "model", "adapter", "projector",
	// "vocab", ... Chat model drafts only come from "model" (or an absent
	// key, in older converters).
	Type string `json:"type"`

	// ContextLength is <arch>.context_length: the trained context size.
	ContextLength int64 `json:"contextLength"`

	// BlockCount is <arch>.block_count.
	BlockCount int64 `json:"blockCount"`

	// HeadCount and HeadCountKV are <arch>.attention.head_count and
	// head_count_kv.
	HeadCount   int64 `json:"headCount"`
	HeadCountKV int64 `json:"headCountKv"`

	// ExpertCount and ExpertUsedCount are <arch>.expert_count and
	// expert_used_count. ExpertCount > 0 marks a Mixture-of-Experts model.
	ExpertCount     int64 `json:"expertCount"`
	ExpertUsedCount int64 `json:"expertUsedCount"`

	// MoE is true when ExpertCount > 0.
	MoE bool `json:"moe"`

	// MTPKeys holds the raw KVs matching the multi-token-prediction /
	// draft-layer hint set (see mtpKeyRe). They are rare and converter
	// specific, so the raw key/value pairs are surfaced verbatim.
	MTPKeys map[string]any `json:"mtpKeys,omitempty"`

	// QuantHint is parsed from the filename (Q4_K_M, IQ3_S, ...), not from
	// the file contents. Empty when the name carries no known quant tag.
	QuantHint string `json:"quantHint"`

	// ExtraKeys lists every remaining metadata key (without values) so a
	// user can see what the file carries at a glance.
	ExtraKeys []string `json:"extraKeys"`
}

// mtpKeyRe matches GGUF keys that describe multi-token-prediction heads and
// draft layers. Keyed on the conventions seen in real files and llama.cpp
// converters:
//   - <arch>.nextn_predict_layers — the MTP head depth emitted by Qwen3-Next
//     / Qwen3.5-style conversions (observed as qwen35.nextn_predict_layers=1
//     in the real models scanned here; DeepSeek conversions emit the same
//     key under deepseek2.*)
//   - draft.* — llama.cpp draft-model GGUFs
//   - n_dense / dense_layer_count — DeepSeek-style dense-vs-MoE layer splits
//
// Matched case-insensitively because converter key naming has drifted.
var mtpKeyRe = regexp.MustCompile(`(?i)(n_dense|dense_layer|draft|nextn_predict|next_predict|mtp|multi.?token)`)

// ReadMetadata opens path read-only, parses the GGUF v2/v3 header and the
// key/value metadata section, and distills it into a Metadata. Only the
// header and KV section are read — tensor data is never touched, so a
// multi-GB model costs a few KB of I/O.
func ReadMetadata(path string) (Metadata, error) {
	meta := Metadata{Path: path}

	info, err := os.Stat(path)
	if err != nil {
		return meta, &GGUFError{Path: path, Detail: "stat", Err: err}
	}
	if info.IsDir() {
		return meta, &GGUFError{Path: path, Detail: "is a directory"}
	}
	meta.FileSize = info.Size()

	file, err := os.Open(path) // #nosec G304 -- paths come from the user's own configured folders
	if err != nil {
		return meta, &GGUFError{Path: path, Detail: "open", Err: err}
	}
	defer file.Close()

	kvs, version, err := parseHeaderAndKVs(file)
	if err != nil {
		var ggufErr *GGUFError
		if errors.As(err, &ggufErr) {
			ggufErr.Path = path
			return meta, err
		}
		return meta, &GGUFError{Path: path, Detail: "parse", Err: err}
	}
	meta.GGUFVersion = version

	meta.Architecture = stringValue(kvs, "general.architecture")
	meta.Type = stringValue(kvs, "general.type")

	arch := meta.Architecture
	meta.ContextLength = intValue(kvs, arch+".context_length")
	meta.BlockCount = intValue(kvs, arch+".block_count")
	meta.HeadCount = intValue(kvs, arch+".attention.head_count")
	meta.HeadCountKV = intValue(kvs, arch+".attention.head_count_kv")
	meta.ExpertCount = intValue(kvs, arch+".expert_count")
	meta.ExpertUsedCount = intValue(kvs, arch+".expert_used_count")
	meta.MoE = meta.ExpertCount > 0

	meta.QuantHint = QuantHintFromFilename(path)

	known := map[string]bool{
		"general.architecture":            true,
		"general.type":                    true,
		arch + ".context_length":          true,
		arch + ".block_count":             true,
		arch + ".attention.head_count":    true,
		arch + ".attention.head_count_kv": true,
		arch + ".expert_count":            true,
		arch + ".expert_used_count":       true,
	}

	for key, value := range kvs {
		if mtpKeyRe.MatchString(key) {
			if meta.MTPKeys == nil {
				meta.MTPKeys = make(map[string]any)
			}
			meta.MTPKeys[key] = value
			continue
		}
		if !known[key] {
			meta.ExtraKeys = append(meta.ExtraKeys, key)
		}
	}
	sortStrings(meta.ExtraKeys)
	return meta, nil
}

// parseHeaderAndKVs reads the GGUF header and every metadata KV entry,
// returning the values keyed by name. Stops after the KV section — tensor
// descriptors and data are not read.
func parseHeaderAndKVs(r io.Reader) (map[string]any, uint32, error) {
	head := make([]byte, 4+4+8+8)
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, 0, fmt.Errorf("reading header: %w", err)
	}
	if string(head[:4]) != ggufMagic {
		return nil, 0, fmt.Errorf("not a GGUF file (magic %q)", trimBytes(head[:4]))
	}
	version := binary.LittleEndian.Uint32(head[4:8])
	if version < 2 || version > ggufMaxVersion {
		return nil, 0, fmt.Errorf("unsupported GGUF version %d (supported: 2, 3)", version)
	}
	tensorCount := binary.LittleEndian.Uint64(head[8:16])
	if tensorCount == 0 {
		return nil, 0, fmt.Errorf("header claims 0 tensors")
	}
	metadataCount := binary.LittleEndian.Uint64(head[16:24])

	if metadataCount > ggufArrayMaxLength {
		return nil, 0, fmt.Errorf("metadata count %d exceeds limit", metadataCount)
	}

	br := newByteReader(r)
	kvs := make(map[string]any, metadataCount)
	for i := uint64(0); i < metadataCount; i++ {
		key, err := br.readString()
		if err != nil {
			return nil, 0, fmt.Errorf("reading metadata key #%d: %w", i, err)
		}
		typeCode, err := br.readUint32()
		if err != nil {
			return nil, 0, fmt.Errorf("reading metadata type for %q: %w", key, err)
		}
		value, err := br.readValue(int(typeCode))
		if err != nil {
			return nil, 0, fmt.Errorf("reading metadata value for %q: %w", key, err)
		}
		kvs[key] = value
	}
	return kvs, version, nil
}

// byteReader wraps an io.Reader with the little-endian primitive reads the
// GGUF format needs.
type byteReader struct {
	r   io.Reader
	tmp [8]byte
}

func newByteReader(r io.Reader) *byteReader { return &byteReader{r: r} }

func (b *byteReader) readFull(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(b.r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (b *byteReader) readUint32() (uint32, error) {
	buf, err := b.readFull(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(buf), nil
}

func (b *byteReader) readUint64() (uint64, error) {
	buf, err := b.readFull(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(buf), nil
}

// readValue decodes one typed GGUF value. Numeric scalars normalize to
// int64/float64, strings to string, bools to bool, arrays to []any.
func (b *byteReader) readValue(typeCode int) (any, error) {
	switch typeCode {
	case ggufKVTypeUint8:
		buf, err := b.readFull(1)
		if err != nil {
			return nil, err
		}
		return int64(buf[0]), nil
	case ggufKVTypeInt8:
		buf, err := b.readFull(1)
		if err != nil {
			return nil, err
		}
		return int64(int8(buf[0])), nil
	case ggufKVTypeUint16:
		v, err := b.readUint16()
		return int64(v), err
	case ggufKVTypeInt16:
		v, err := b.readUint16()
		return int64(int16(v)), err
	case ggufKVTypeUint32:
		v, err := b.readUint32()
		return int64(v), err
	case ggufKVTypeInt32:
		v, err := b.readUint32()
		return int64(int32(v)), err
	case ggufKVTypeFloat32:
		buf, err := b.readFull(4)
		if err != nil {
			return nil, err
		}
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(buf))), nil
	case ggufKVTypeBool:
		buf, err := b.readFull(1)
		if err != nil {
			return nil, err
		}
		return buf[0] != 0, nil
	case ggufKVTypeString:
		return b.readString()
	case ggufKVTypeUint64:
		v, err := b.readUint64()
		if err != nil {
			return nil, err
		}
		if v > uint64(1)<<62 {
			// keep the number meaningful without lying about width
			return float64(v), nil
		}
		return int64(v), nil
	case ggufKVTypeInt64:
		v, err := b.readUint64()
		return int64(v), err
	case ggufKVTypeFloat64:
		buf, err := b.readFull(8)
		if err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(buf)), nil
	case ggufKVTypeArray:
		elemCode, err := b.readUint32()
		if err != nil {
			return nil, err
		}
		count, err := b.readUint64()
		if err != nil {
			return nil, err
		}
		if count > ggufArrayMaxLength {
			return nil, fmt.Errorf("array length %d exceeds limit", count)
		}
		items := make([]any, 0, count)
		for i := uint64(0); i < count; i++ {
			item, err := b.readValue(int(elemCode))
			if err != nil {
				return nil, fmt.Errorf("element %d: %w", i, err)
			}
			items = append(items, item)
		}
		return items, nil
	default:
		return nil, fmt.Errorf("unknown GGUF value type %d", typeCode)
	}
}

func (b *byteReader) readUint16() (uint16, error) {
	buf, err := b.readFull(2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(buf), nil
}

// readString reads a GGUF string: uint64 length followed by UTF-8 bytes.
func (b *byteReader) readString() (string, error) {
	length, err := b.readUint64()
	if err != nil {
		return "", err
	}
	if length > ggufArrayMaxLength {
		return "", fmt.Errorf("string length %d exceeds limit", length)
	}
	buf, err := b.readFull(int(length))
	if err != nil {
		return "", err
	}
	return string(buf), nil
}

func stringValue(kvs map[string]any, key string) string {
	if v, ok := kvs[key].(string); ok {
		return v
	}
	return ""
}

func intValue(kvs map[string]any, key string) int64 {
	switch v := kvs[key].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case uint64:
		return int64(v)
	default:
		return 0
	}
}

func trimBytes(b []byte) string {
	return strings.TrimRight(string(b), "\x00")
}

func sortStrings(s []string) {
	sort.Strings(s)
}
