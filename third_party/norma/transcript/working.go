package transcript

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/Autumn-27/norma/llm"
)

type workingSnapshot struct {
	LastUUID string        `json:"last_uuid"`
	Messages []llm.Message `json:"messages"`
}

func (w *Writer) LastUUID() string { w.mu.Lock(); defer w.mu.Unlock(); return w.last }

func (s *Store) SaveWorkingMessages(id, last string, msgs []llm.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.MainPath(id) + ".working.json"
	f, err := os.CreateTemp(filepath.Dir(path), ".working-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := json.NewEncoder(f).Encode(workingSnapshot{last, msgs}); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func (s *Store) WorkingMessages(id string, recs []Record) ([]llm.Message, bool, error) {
	data, err := os.ReadFile(s.MainPath(id) + ".working.json")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			msgs, _ := Messages(recs)
			return msgs, false, nil
		}
		return nil, false, err
	}
	var snapshot workingSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, false, err
	}
	if snapshot.LastUUID != LastUUID(recs) {
		msgs, _ := Messages(recs)
		return msgs, false, nil
	}
	return snapshot.Messages, true, nil
}
