package objectstore

import (
	"context"
	"os"
	"testing"

	"github.com/anywaye/darkroom/internal/checksum"
)

// TestIntegrationRoundTrip exercises a real S3-compatible bucket end to end:
// presign a PUT, write bytes through it, head, get, hash, delete. It is the
// round-trip coverage "Not done" names as missing, and it is guarded so it
// never runs in CI without a bucket: a test that could reach the network from a
// default `go test ./...` is a test that fails on every laptop.
//
// To run it:
//
//	DARKROOM_TEST_S3_BUCKET=... \
//	DARKROOM_TEST_S3_ENDPOINT=...  (optional; defaults to AWS) \
//	DARKROOM_TEST_S3_REGION=...    (optional; defaults to us-east-1) \
//	DARKROOM_TEST_S3_ACCESS=... \
//	DARKROOM_TEST_S3_SECRET=... \
//	go test ./internal/objectstore/ -run TestIntegrationRoundTrip -v
func TestIntegrationRoundTrip(t *testing.T) {
	bucket := os.Getenv("DARKROOM_TEST_S3_BUCKET")
	access := os.Getenv("DARKROOM_TEST_S3_ACCESS")
	secret := os.Getenv("DARKROOM_TEST_S3_SECRET")
	if bucket == "" || access == "" || secret == "" {
		t.Skip("DARKROOM_TEST_S3_BUCKET/ACCESS/SECRET not set; integration test skipped")
	}
	region := os.Getenv("DARKROOM_TEST_S3_REGION")
	if region == "" {
		region = "us-east-1"
	}
	endpoint := os.Getenv("DARKROOM_TEST_S3_ENDPOINT")

	cfg := S3Config{
		Bucket:   bucket,
		Region:   region,
		Endpoint: endpoint,
		Access:   access,
		Secret:   secret,
	}
	store, err := NewS3ForTest(cfg)
	if err != nil {
		t.Fatalf("NewS3ForTest: %v", err)
	}

	ctx := context.Background()
	key := "darkroom-integration-test/original"
	body := []byte("hello darkroom")
	if err := store.Put(ctx, key, body, "text/plain"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, key) })

	meta, err := store.Head(ctx, key)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if meta.ByteSize != int64(len(body)) {
		t.Fatalf("Head size = %d, want %d", meta.ByteSize, len(body))
	}
	got, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("Get body mismatch")
	}
	if checksum.SHA256Hex(body) != checksum.SHA256Hex(got) {
		t.Fatalf("checksum mismatch")
	}
}
