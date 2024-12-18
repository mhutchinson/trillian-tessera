// Copyright 2024 The Tessera authors. All Rights Reserved.
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

package storage

import (
	"context"

	tessera "github.com/transparency-dev/trillian-tessera"
	"github.com/transparency-dev/trillian-tessera/internal/driver"
)

// Driver is an envelope containing implementations of the lower level operations used in Tessera.
// Personalities are not able to use any of the contents of this object directly, because the types
// for all of the sub-structs are defined in an internal directory. This is intentional.
//
// Expected usage it to acquire a Driver and then pass it to one of the New* lifecycle creators below.
type Driver struct {
	Readers   driver.Readers
	Appenders driver.Appenders
}

// LogReader provides read-only access to the log.
type LogReader struct {
	// TODO(mhutchinson): Write out the full docs for this here
	ReadCheckpoint func(ctx context.Context) ([]byte, error)

	// TODO(mhutchinson): Write out the full docs for this here
	ReadTile func(ctx context.Context, level, index uint64, p uint8) ([]byte, error)

	// TODO(mhutchinson): Write out the full docs for this here
	ReadEntryBundle func(ctx context.Context, index uint64, p uint8) ([]byte, error)
}

// Appender allows new entries to be added to the log, and the contents of the log to be read.
type Appender struct {
	LogReader

	// Add adds a new entry to be sequenced.
	// TODO(mhutchinson): Write out the full docs for this here
	Add func(ctx context.Context, entry *tessera.Entry) tessera.IndexFuture
}

// NewAppender returns an Appender, which allows a personality to incrementally append new
// leaves to the log and to read from it.
//
// TODO(mhutchinson): work out how to put the dedupe stuff into this method params.
func NewAppender(d Driver) Appender {
	return Appender{
		LogReader: newLogReader(d),
		Add:       d.Appenders.Add,
	}
}

func newLogReader(d Driver) LogReader {
	return LogReader{
		ReadCheckpoint:  d.Readers.ReadCheckpoint,
		ReadTile:        d.Readers.ReadTile,
		ReadEntryBundle: d.Readers.ReadEntryBundle,
	}
}
