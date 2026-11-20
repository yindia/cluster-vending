package results

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// compressedSlack is how far the compressed bytes of a bounded object may
// exceed twice the bound (gzip's framing and incompressible content).
const compressedSlack = 4096

// Encode is the object body of msg: protojson, gzip compressed. A message
// whose protojson is above maxBytes (zero: DefaultMaxBytes) is ErrTooLarge.
func Encode(msg proto.Message, maxBytes int) ([]byte, error) {
	if msg == nil || !msg.ProtoReflect().IsValid() {
		return nil, errors.New("results: nothing to encode")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	raw, err := protojson.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("results: encode %s: %w", msg.ProtoReflect().Descriptor().FullName(), err)
	}
	if len(raw) > maxBytes {
		return nil, fmt.Errorf("%w: %d bytes encoded, at most %d", ErrTooLarge, len(raw), maxBytes)
	}
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	if _, err := w.Write(raw); err != nil {
		return nil, fmt.Errorf("results: compress: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("results: compress: %w", err)
	}
	return out.Bytes(), nil
}

// Decode reads an object body (Encode's) into into. Neither the compressed
// nor the inflated bytes may pass maxBytes (zero: DefaultMaxBytes), so a
// small object cannot inflate without bound: ErrTooLarge. Anything that is
// not gzip protojson of into is ErrCorrupt. Fields this build does not know
// (a newer writer's) are ignored.
func Decode(body io.Reader, maxBytes int, into proto.Message) error {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	limit := int64(2*maxBytes + compressedSlack)
	packed, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return err // the transport's: the caller classifies it
	}
	if int64(len(packed)) > limit {
		return fmt.Errorf("%w: the object is above %d bytes compressed", ErrTooLarge, limit)
	}
	zr, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		return fmt.Errorf("%w: not gzip: %w", ErrCorrupt, err)
	}
	raw, err := io.ReadAll(io.LimitReader(zr, int64(maxBytes)+1))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	if len(raw) > maxBytes {
		return fmt.Errorf("%w: more than %d bytes inflated", ErrTooLarge, maxBytes)
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, into); err != nil {
		return fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	return nil
}
