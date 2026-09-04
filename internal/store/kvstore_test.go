package store_test

import (
	"testing"

	"github.com/andrianbdn/oddk/internal/store/kvstore"
)

// System parameters exist with their defaults from the first open, round-trip
// through SetInt, and — the property the health checker relies on — a deleted
// row makes RequiredInt fall back to the registered default instead of
// failing a long-running daemon.
func TestKVStoreSystemParameters(t *testing.T) {
	st := newTestStore(t)
	key := kvstore.KeyDiskSpaceThresholdBytes

	def, err := st.KV.GetInt(key)
	if err != nil || def <= 0 {
		t.Fatalf("GetInt(%s) on a fresh store = %d, %v; want the registered default", key, def, err)
	}

	if err := st.KV.SetInt(key, def*2); err != nil {
		t.Fatalf("SetInt: %v", err)
	}
	if got, err := st.KV.GetInt(key); err != nil || got != def*2 {
		t.Fatalf("GetInt after SetInt = %d, %v; want %d", got, err, def*2)
	}
	if got := st.KV.RequiredInt(key); got != def*2 {
		t.Fatalf("RequiredInt = %d, want %d", got, def*2)
	}

	if err := st.KV.DeleteInt(key); err != nil {
		t.Fatalf("DeleteInt: %v", err)
	}
	if _, err := st.KV.GetInt(key); err == nil {
		t.Fatal("GetInt after DeleteInt should fail")
	}
	if got := st.KV.RequiredInt(key); got != def {
		t.Fatalf("RequiredInt after the row was deleted = %d, want the default %d", got, def)
	}

	if _, err := st.KV.Get(kvstore.Key("no.such.key.str")); err == nil {
		t.Fatal("Get of an unknown key should fail")
	}
}
