// Package sqlite provides the durable, migration-checked SQLite bootstrap used
// by the canonical RKC store. It deliberately exposes no general SQL execution
// API; higher storage layers own repository reads and writes.
//
// Open rejects symlinked paths, creates new files exclusively at mode 0600,
// requires mode 0600 plus an owner-only immediate parent, retains pinned file
// and parent descriptors until Close, and verifies their device/inode identity
// around database work. Same-UID processes are inside the trust boundary:
// callers must not grant another same-UID process authority to rename or replace
// the database directory while Database is open.
//
// Open performs fixed-size structural, ownership, migration, and connection
// checks. Check is the explicit full maintenance audit and may scan all database
// pages and foreign keys; callers should give it a deadline and run it inside
// the repository resource guard for large catalogues.
package sqlite
