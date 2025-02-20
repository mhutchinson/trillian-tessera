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
	"strings"
	"sync"

	tessera "github.com/transparency-dev/trillian-tessera"
	"github.com/transparency-dev/trillian-tessera/internal/parse"
	"k8s.io/klog/v2"
)

type ProofFetchFn func(ctx context.Context, from, to uint64) [][]byte

func NewWitnessGateway(group tessera.WitnessGroup, client *http.Client, fetchProof ProofFetchFn) WitnessGateway {
	urls := group.URLs()
	slices.Sort(urls)
	urls = slices.Compact(urls)
	witnesses := make([]witness, len(urls))
	postFnImpl := func(ctx context.Context, url string, body string) (resp *http.Response, err error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		return client.Do(req)
	}
	for _, u := range urls {
		witnesses = append(witnesses, witness{
			url:        u,
			size:       0,
			post:       postFnImpl,
			fetchProof: fetchProof,
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
	// TODO(mhutchinson): also set a deadline?
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var waitGroup sync.WaitGroup
	_, size, err := parse.CheckpointUnsafe(cp)
	if err != nil {
		return nil, fmt.Errorf("failed to parse checkpoint from log: %v", err)
	}

	type sigOrErr struct {
		sig []byte
		err error
	}
	results := make(chan sigOrErr)

	// Kick off a goroutine for each witness and send result to results chan
	for _, w := range wg.witnesses {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			sig, err := w.update(ctx, cp, size)
			results <- sigOrErr{
				sig: sig,
				err: err,
			}
		}()
	}

	go func() {
		waitGroup.Wait()
		close(results)
	}()

	// Consume the results coming back from each witness
	var sigBlock bytes.Buffer
	sigBlock.Write(cp)
	for r := range results {
		if r.err != nil {
			err = errors.Join(err, r.err)
			continue
		}
		// Add new signature to the new note we're building
		sigBlock.Write(r.sig)
		sigBlock.WriteRune('\n')

		// See whether the group is satisfied now
		if newCp := sigBlock.Bytes(); wg.group.Satisfied(newCp) {
			return newCp, nil
		}
	}

	// We can only get here if all witnesses have returned and we're still not satisfied.
	return nil, err
}

type postFn func(ctx context.Context, url, body string) (resp *http.Response, err error)

type witness struct {
	url        string
	size       uint64
	post       postFn
	fetchProof ProofFetchFn
}

func (w witness) update(ctx context.Context, cp []byte, size uint64) ([]byte, error) {
	proof := w.fetchProof(ctx, w.size, size)

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

	resp, err := w.post(ctx, w.url, body)
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

	// TODO(mhutchinson): this will need to take a verifier to understand which sig to extract.
	// For now, it just takes the last signature line.
	lines := bytes.Split(rb, []byte{'\n'})
	for _, l := range lines {
		if len(l) > 0 {
			return l, nil
		}
	}

	return nil, errors.New("failed to find signature in response body")
}
