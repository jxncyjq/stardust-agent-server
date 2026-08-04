package browser

import (
	"testing"
	"time"
)

// fakePersist 记录写穿调用。
type fakePersist struct {
	saved   map[string]SessionRecord
	touched map[string]time.Time
	deleted []string
}

func newFakePersist() *fakePersist {
	return &fakePersist{saved: map[string]SessionRecord{}, touched: map[string]time.Time{}}
}
func (f *fakePersist) Save(rec SessionRecord) error { f.saved[rec.ID] = rec; return nil }
func (f *fakePersist) Touch(id, activeURL string, lastUsed time.Time) error {
	f.touched[id] = lastUsed
	return nil
}
func (f *fakePersist) Get(id string) (SessionRecord, bool, error) {
	r, ok := f.saved[id]
	return r, ok, nil
}
func (f *fakePersist) List() ([]SessionRecord, error) {
	var out []SessionRecord
	for _, r := range f.saved {
		out = append(out, r)
	}
	return out, nil
}
func (f *fakePersist) Delete(id string) error { f.deleted = append(f.deleted, id); return nil }

func TestSessionStoreWritesThroughOnCreate(t *testing.T) {
	p := newFakePersist()
	st := NewSessionStore()
	st.SetPersist(p)
	sess := st.Create("task-1")
	if _, ok := p.saved[sess.ID]; !ok {
		t.Fatalf("Create did not persist session %q", sess.ID)
	}
}

func TestSessionStoreDeleteWritesThrough(t *testing.T) {
	p := newFakePersist()
	st := NewSessionStore()
	st.SetPersist(p)
	sess := st.Create("task-1")
	st.Delete(sess.ID)
	if len(p.deleted) != 1 || p.deleted[0] != sess.ID {
		t.Fatalf("Delete did not persist removal: %v", p.deleted)
	}
}

// persist 为 nil（默认）时不 panic——Phase 1/2 纯内存路径不变。
func TestSessionStoreNilPersistNoPanic(t *testing.T) {
	st := NewSessionStore()
	sess := st.Create("task-1")
	st.Delete(sess.ID)
	_ = sess
}
