// Copyright 2025 The Tessera authors. All Rights Reserved.
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
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"

	tessera "github.com/transparency-dev/trillian-tessera"
	"github.com/transparency-dev/trillian-tessera/internal/parse"
	"k8s.io/klog/v2"
)

func NewWitnessGateway(group tessera.WitnessGroup, client *http.Client) WitnessGateway {
	urls := group.URLs()
	slices.Sort(urls)
	urls = slices.Compact(urls)
	witnesses := make([]witness, len(urls))
	for _, u := range urls {
		witnesses = append(witnesses, witness{
			url:  u,
			size: 0,
			post: client.Post,
		})
	}
	return WitnessGateway{
		group:     group,
		witnesses: witnesses,
	}
}

type WitnessGateway struct {
	group     tessera.WitnessGroup
	witnesses []witness
}

func (wg WitnessGateway) Witness(ctx context.Context, cp []byte) ([]byte, error) {
	var waitGroup sync.WaitGroup
	_, size, err := parse.CheckpointUnsafe(cp)
	if err != nil {
		return nil, fmt.Errorf("failed to parse checkpoint from log: %v", err)
	}

	for _, w := range wg.witnesses {
		waitGroup.Add(1)

		go func() {
			defer waitGroup.Done()
			w.update(cp, size)
		}()

	}
	waitGroup.Wait()
	return nil, errors.New("not done yet m8")
}

type PostFn func(url, contentType string, body io.Reader) (resp *http.Response, err error)

type ProofFetchFn func(from, to uint64) [][]byte

type witness struct {
	url        string
	size       uint64
	post       PostFn
	fetchProof ProofFetchFn
}

func (w witness) update(cp []byte, size uint64) ([]byte, error) {
	proof := w.fetchProof(w.size, size)

	// The request body MUST be a sequence of
	// - a previous size line,
	// - zero or more consistency proof lines,
	// - and an empty line,
	// - followed by a [checkpoint][].
	body := fmt.Sprintf("old %d\n", w.size)
	for _, p := range proof {
		body += base64.StdEncoding.EncodeToString(p) + "\n"
	}
	body += "\n"
	body += string(cp)

	resp, err := w.post(w.url, "", bytes.NewReader([]byte(body)))
	if err != nil {
		klog.Warningf("Failed to post to witness at %q: %v", w.url, err)
	}

	// TODO: do a case statement for these cases
	// In particular this needs to handle the witness size being wrong:
	//  - update the size
	//  - try this method again
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("got bad status code: %v", resp.StatusCode)
	}
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %v", err)
	}
	_ = resp.Body.Close()

	lines := bytes.Split(rb, []byte{'\n'})
	for _, l := range lines {
		if len(l) > 0 {
			return l, nil
		}
	}

	return nil, errors.New("failed to find signature in response body")
}
