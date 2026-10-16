package state

import (
	"path/filepath"
	"testing"
)

func TestLockReadOnlyAndDurability(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state?# literal")
	writer, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if another, err := Open(dir, false); err == nil {
		another.Close()
		t.Fatal("second writer acquired lock")
	}
	value := struct{ UID int }{42}
	if err = writer.Put("test", "checkpoint", value); err != nil {
		t.Fatal(err)
	}
	reader, err := Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ UID int }
	if ok, err := reader.Get("test", "checkpoint", &got); err != nil || !ok || got.UID != 42 {
		t.Fatal("read-only checkpoint unavailable", err)
	}
	reader.Close()
	writer.Close()
	reopened, err := Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got.UID = 0
	if ok, err := reopened.Get("test", "checkpoint", &got); err != nil || !ok || got.UID != 42 {
		t.Fatal("checkpoint not durable", err)
	}
}

func TestNamespaceChangeRollsBackAsOneTransaction(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.Put("folder", "Old", map[string]string{"name": "Old"})
	err = st.Apply([]Change{{Kind: "folder", ID: "Old", Delete: true}, {Kind: "folder", ID: "New", Value: map[string]string{"name": "New"}}, {Kind: "broken", ID: "bad", Value: make(chan int)}})
	if err == nil {
		t.Fatal("invalid transaction accepted")
	}
	var got map[string]string
	if found, err := st.Get("folder", "Old", &got); err != nil || !found || got["name"] != "Old" {
		t.Fatal("old mapping not rolled back", err)
	}
	if found, err := st.Get("folder", "New", &got); err != nil || found {
		t.Fatal("new mapping partially committed", err)
	}
}
