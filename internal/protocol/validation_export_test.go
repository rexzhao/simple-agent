package protocol

import "testing"

func TestExportedSyncDTOValidatorsMatchWireRules(t *testing.T) {
	if err := ValidateChangeOperation(ChangeOperation{Op: "   "}); err == nil {
		t.Fatal("whitespace operation was accepted")
	}
	if err := ValidateChangeOperation(ChangeOperation{
		Op:  "upsert",
		Raw: []byte(`{"op":"remove"}`),
	}); err == nil {
		t.Fatal("operation/raw op mismatch was accepted")
	}

	blob := BlobDescriptor{
		ID: "blob-1", URL: "/api/blobs/blob-1", ContentType: "application/json",
		Size: 1, SHA256: "hash", ETag: "etag", ExpiresAt: "2025-01-01T00:00:00Z",
	}
	if err := ValidateBlobDescriptor(blob); err != nil {
		t.Fatal(err)
	}
	blob.Size = maxSafeJSONInteger + 1
	if err := ValidateBlobDescriptor(blob); err == nil {
		t.Fatal("unsafe blob size was accepted")
	}
	blob.Size = 1
	blob.ExpiresAt = "2025-01-01 00:00:00Z"
	if err := ValidateBlobDescriptor(blob); err == nil {
		t.Fatal("non-RFC3339 blob expiry was accepted")
	}

	if err := ValidateSnapshotContent(SnapshotContent{Inline: []byte(`{"items":[]}`)}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSnapshotContent(SnapshotContent{Inline: []byte(`[]`)}); err == nil {
		t.Fatal("non-object inline snapshot was accepted")
	}
	if err := ValidateResourceKey(ResourceKey{Type: ResourceTypeProjectIndex, ID: " "}); err == nil {
		t.Fatal("whitespace resource ID was accepted")
	}
	if err := ValidateResumeToken(&ResumeToken{StreamEpoch: " ", Sequence: "0"}); err == nil {
		t.Fatal("whitespace resume epoch was accepted")
	}
}
