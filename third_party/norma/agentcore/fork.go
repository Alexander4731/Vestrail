package agentcore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

type TranscriptInfo struct {
	Exists             bool
	MessageCount       int
	LastInputTokens    int
	HasCompactBound    bool
	HasWorkingSnapshot bool
}

func validForkID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\") && filepath.VolumeName(id) == ""
}

func InspectTranscript(store *transcript.Store, sessionID string) (TranscriptInfo, error) {
	var info TranscriptInfo
	if store == nil || !validForkID(sessionID) {
		return info, fmt.Errorf("agentcore: invalid transcript inspection")
	}
	if _, err := os.Stat(store.MainPath(sessionID)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return info, nil
		}
		return info, err
	}
	info.Exists = true
	recs, err := store.Load(store.MainPath(sessionID))
	if err != nil {
		return info, err
	}
	working, fresh, err := store.WorkingMessages(sessionID, recs)
	if err != nil {
		return info, err
	}
	info.HasWorkingSnapshot = fresh
	if fresh {
		info.MessageCount = len(working)
	}
	for _, r := range recs {
		if r.Message != nil {
			if !fresh {
				info.MessageCount++
			}
			if llm.IsBoundaryMarker(*r.Message) {
				info.HasCompactBound = true
			}
		}
		if r.Type == "boundary" {
			info.HasCompactBound = true
		}
		// Normalized input already contains cache reads/writes.
		if r.Usage != nil && r.Usage.InputTokens > 0 {
			info.LastInputTokens = r.Usage.InputTokens
		}
	}
	return info, nil
}

func (s *Session) ForkFrom(sourceSessionID, newSessionID string) error {
	if s.store == nil || !validForkID(sourceSessionID) || !validForkID(newSessionID) || sourceSessionID == newSessionID {
		return fmt.Errorf("agentcore: ForkFrom requires a store and distinct valid ids")
	}
	info, err := InspectTranscript(s.store, sourceSessionID)
	if err != nil {
		return err
	}
	if !info.Exists || info.MessageCount == 0 {
		return fmt.Errorf("agentcore: source history is missing or empty")
	}
	recs, err := s.store.Load(s.store.MainPath(sourceSessionID))
	if err != nil {
		return err
	}
	msgs, _, err := s.store.WorkingMessages(sourceSessionID, recs)
	if err != nil {
		return err
	}
	msgs = cleanResumed(msgs)
	last, err := s.store.SeedSession(newSessionID, msgs)
	if err != nil {
		return err
	}
	s.messages = msgs
	s.SeedCompactionReference(info.LastInputTokens)
	s.usage = llm.Usage{}
	s.sessionID = newSessionID
	s.opts.SessionID = newSessionID
	s.writer = s.store.ResumeWriter(newSessionID, last)
	return nil
}

// UpdateRunLimits changes only per-prompt controls between serialized prompts.
// Keeping the live Session retains ephemeral micro-compaction and deferred state.
func (s *Session) UpdateRunOptions(opts Options) {
	s.opts.MaxTurns = opts.MaxTurns
	s.opts.MaxDuration = opts.MaxDuration
	s.opts.SettleDuration = opts.SettleDuration
	s.opts.MaxTokens = opts.MaxTokens
	s.opts.NonStreaming = opts.NonStreaming
	s.opts.Settlement = opts.Settlement
	s.opts.Hooks = opts.Hooks
	s.opts.PreserveReasoningHistory = opts.PreserveReasoningHistory
}

func (s *Session) CheckpointWorkingSet() error {
	if s.store == nil || s.writer == nil {
		return fmt.Errorf("agentcore: working checkpoint requires a transcript")
	}
	if err := s.writer.Err(); err != nil {
		return err
	}
	return s.store.SaveWorkingMessages(s.sessionID, s.writer.LastUUID(), s.messages)
}

// SeedCompactionReference preserves the source token scale on the first fork request.
func (s *Session) SeedCompactionReference(tokens int) {
	s.lastInputTokens = tokens
	if c, ok := s.compactor.(interface{ SeedMicroReference(int) }); ok {
		c.SeedMicroReference(tokens)
	}
}
