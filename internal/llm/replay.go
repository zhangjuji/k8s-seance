package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// recording is the content of one fixture file, <sanitized fixtureID>.json.
type recording struct {
	FixtureID string         `json:"fixtureId"`
	Turns     []recordedTurn `json:"turns"`
}

// recordedTurn is one spirit reply. Cause and LastUser are kept for reading
// the file by hand; replay looks only at TurnIndex.
type recordedTurn struct {
	TurnIndex int           `json:"turnIndex"`
	Cause     string        `json:"cause"`
	LastUser  string        `json:"lastUser"`
	Reply     string        `json:"reply"`
	Usage     recordedUsage `json:"usage"`
}

type recordedUsage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
}

// fixtureFileName maps a fixture ID to its file name. The ID is lowercased and
// every byte outside [a-z0-9-_] becomes '-', so an ID cannot contain path
// separators. ok is false when nothing usable is left, for example for "".
func fixtureFileName(id string) (name string, ok bool) {
	var b strings.Builder
	for _, r := range strings.ToLower(id) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "", false
	}
	return b.String() + ".json", true
}

// ReplayProvider replays recorded replies from FS. Files are read lazily and kept
// in a cache. A request is answered from the recording when the file for
// FixtureID exists, its fixtureId matches, and it has a turn with TurnIndex.
// Any other request goes to Fallback, which is MockProvider{} when nil.
//
// A file that is missing, unreadable or not valid JSON counts as a miss, and the
// request goes to Fallback with no error. The bad file is not reported.
// Delay is the pause after each streamed fragment. Zero means no sleep.
type ReplayProvider struct {
	FS       fs.FS
	Fallback Provider
	Delay    time.Duration

	mu    sync.Mutex
	cache map[string]*recording // nil value: file unusable
}

var _ Provider = (*ReplayProvider)(nil)

// Stream implements Provider. On a hit it streams the recorded reply in
// chunkRunes-sized fragments and returns the recorded usage. It returns the
// context error when ctx is done, and the error from onDelta when onDelta fails.
func (p *ReplayProvider) Stream(ctx context.Context, req ChatRequest, onDelta func(string) error) (Usage, error) {
	if onDelta == nil {
		return Usage{}, errors.New("replay: nil onDelta")
	}
	turn, ok := p.lookup(req)
	if !ok {
		return p.fallback().Stream(ctx, req, onDelta)
	}
	if err := ctx.Err(); err != nil {
		return Usage{}, err
	}

	runes := []rune(turn.Reply)
	for start := 0; start < len(runes); start += chunkRunes {
		if err := ctx.Err(); err != nil {
			return Usage{}, err
		}
		end := min(start+chunkRunes, len(runes))
		if err := onDelta(string(runes[start:end])); err != nil {
			return Usage{}, err
		}
		if p.Delay > 0 {
			timer := time.NewTimer(p.Delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return Usage{}, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return Usage{
		InputTokens:  turn.Usage.InputTokens,
		OutputTokens: turn.Usage.OutputTokens,
	}, nil
}

func (p *ReplayProvider) fallback() Provider {
	if p.Fallback != nil {
		return p.Fallback
	}
	return MockProvider{}
}

// lookup returns the recorded turn for req, if there is one.
func (p *ReplayProvider) lookup(req ChatRequest) (recordedTurn, bool) {
	if req.FixtureID == "" {
		return recordedTurn{}, false
	}
	name, ok := fixtureFileName(req.FixtureID)
	if !ok {
		return recordedTurn{}, false
	}
	rec := p.load(name)
	if rec == nil || rec.FixtureID != req.FixtureID {
		return recordedTurn{}, false
	}
	for _, t := range rec.Turns {
		if t.TurnIndex == req.TurnIndex {
			return t, true
		}
	}
	return recordedTurn{}, false
}

// load returns the parsed recording name, reading it on first use. It returns nil
// when the file cannot be used. The result is cached either way.
func (p *ReplayProvider) load(name string) *recording {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cache == nil {
		p.cache = make(map[string]*recording)
	}
	if rec, ok := p.cache[name]; ok {
		return rec
	}
	rec := readRecording(p.FS, name)
	p.cache[name] = rec
	return rec
}

// readRecording returns nil when fsys is nil, the file is missing or unreadable,
// or the content is not valid JSON.
func readRecording(fsys fs.FS, name string) *recording {
	if fsys == nil {
		return nil
	}
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil
	}
	var rec recording
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil
	}
	return &rec
}

// Recorder passes each request to Inner and writes each successful reply to
// Dir/<sanitized fixtureID>.json. A turn with the same TurnIndex as an existing
// one replaces it. Other turns in the file are kept. Failed calls are not recorded.
// A file that exists but cannot be decoded, or that belongs to another fixture ID,
// is an error and is not overwritten.
type Recorder struct {
	Inner Provider
	Dir   string

	mu sync.Mutex // serializes file writes
}

var _ Provider = (*Recorder)(nil)

// Stream implements Provider. It streams Inner's output through onDelta, then
// saves the full reply. If the save fails, the usage is returned with the error.
func (r *Recorder) Stream(ctx context.Context, req ChatRequest, onDelta func(string) error) (Usage, error) {
	if r.Inner == nil {
		return Usage{}, errors.New("record: nil inner provider")
	}
	if onDelta == nil {
		return Usage{}, errors.New("record: nil onDelta")
	}
	var reply strings.Builder
	usage, err := r.Inner.Stream(ctx, req, func(s string) error {
		reply.WriteString(s)
		return onDelta(s)
	})
	if err != nil {
		return usage, err
	}
	if err := r.save(req, reply.String(), usage); err != nil {
		return usage, err
	}
	return usage, nil
}

// save upserts one turn into the fixture file.
func (r *Recorder) save(req ChatRequest, reply string, usage Usage) error {
	name, ok := fixtureFileName(req.FixtureID)
	if !ok {
		return fmt.Errorf("record: fixture ID %q has no usable characters", req.FixtureID)
	}
	if r.Dir == "" {
		return errors.New("record: empty output directory")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	path := filepath.Join(r.Dir, name)
	rec := recording{FixtureID: req.FixtureID}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &rec); err != nil {
			return fmt.Errorf("record: decode %s: %w", path, err)
		}
		if rec.FixtureID != req.FixtureID {
			return fmt.Errorf("record: %s belongs to fixture %q, not %q", path, rec.FixtureID, req.FixtureID)
		}
	case errors.Is(err, fs.ErrNotExist):
		// First turn for this fixture.
	default:
		return fmt.Errorf("record: read %s: %w", path, err)
	}

	turn := recordedTurn{
		TurnIndex: req.TurnIndex,
		Cause:     string(req.Cause),
		LastUser:  lastUserContent(req.Messages),
		Reply:     reply,
		Usage:     recordedUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens},
	}
	rec.Turns = upsertTurn(rec.Turns, turn)
	slices.SortStableFunc(rec.Turns, func(a, b recordedTurn) int { return a.TurnIndex - b.TurnIndex })

	out, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("record: encode %s: %w", path, err)
	}
	return writeFileAtomic(r.Dir, path, append(out, '\n'))
}

// upsertTurn replaces the turn with the same TurnIndex, or appends turn when none matches.
func upsertTurn(turns []recordedTurn, turn recordedTurn) []recordedTurn {
	for i := range turns {
		if turns[i].TurnIndex == turn.TurnIndex {
			turns[i] = turn
			return turns
		}
	}
	return append(turns, turn)
}

// lastUserContent returns the content of the last user message, or "".
func lastUserContent(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

// writeFileAtomic writes data to a temp file in dir and renames it to path, so
// readers never see a partial file. dir is created if it does not exist.
func writeFileAtomic(dir, path string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("record: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("record: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("record: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("record: close temp file: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("record: chmod temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("record: rename to %s: %w", path, err)
	}
	return nil
}
