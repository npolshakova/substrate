// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package api

import (
	"context"
	"sync"
)

type MemorySource struct {
	mu       sync.RWMutex
	snapshot *Snapshot
	watchers []chan *Snapshot
}

func NewMemorySource(snapshot *Snapshot) *MemorySource {
	return &MemorySource{snapshot: cloneSnapshot(snapshot)}
}

func (s *MemorySource) GetSnapshot(context.Context) (*Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneSnapshot(s.snapshot), nil
}

func (s *MemorySource) WatchSnapshot(ctx context.Context) <-chan *Snapshot {
	ch := make(chan *Snapshot, 1)
	s.mu.Lock()
	s.watchers = append(s.watchers, ch)
	current := cloneSnapshot(s.snapshot)
	s.mu.Unlock()
	ch <- current

	go func() {
		<-ctx.Done()
		s.mu.Lock()
		defer s.mu.Unlock()
		for i, watcher := range s.watchers {
			if watcher == ch {
				s.watchers = append(s.watchers[:i], s.watchers[i+1:]...)
				close(ch)
				return
			}
		}
	}()

	return ch
}

func (s *MemorySource) SetSnapshot(snapshot *Snapshot) {
	next := cloneSnapshot(snapshot)
	s.mu.Lock()
	s.snapshot = next
	watchers := append([]chan *Snapshot(nil), s.watchers...)
	s.mu.Unlock()

	for _, watcher := range watchers {
		select {
		case watcher <- cloneSnapshot(next):
		default:
		}
	}
}

func cloneSnapshot(snapshot *Snapshot) *Snapshot {
	if snapshot == nil {
		return nil
	}
	out := &Snapshot{Policies: make([]EgressPolicy, len(snapshot.Policies))}
	copy(out.Policies, snapshot.Policies)
	return out
}
