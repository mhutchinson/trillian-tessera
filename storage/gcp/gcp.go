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

// Package gcp contains a GCP-based storage implementation for Tessera.
package gcp

import "errors"

// Storage is a GCP based storage implementation for Tessera.
type Storage struct{}

// New creates a new instance of the GCP based Storage.
func New() (*Storage, error) {
	return nil, errors.New("unimplemented")
}