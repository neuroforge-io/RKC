// Package rkcstore defines the canonical transactional storage boundary for
// Repository Knowledge Representation snapshots.
//
// A build is private until Commit succeeds. Committed snapshots are immutable,
// and a successful commit publishes both the snapshot and its repository's
// current pointer as one operation. Implementations must return defensive
// copies: callers never receive storage-owned maps, slices, or pointers.
package rkcstore
