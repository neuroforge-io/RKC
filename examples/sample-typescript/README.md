# Sample TypeScript service

The `AuthService` exposes a small login flow used by the mixed-language smoke
test. It delegates password verification to an explicit credential store so a
caller must supply its own password-hashing and constant-time comparison
policy; the example never treats a username lookup as authentication.

See [the implementation](src/auth.ts).

---
_RKC is open source, published and maintained by **NeuroForgeIO**, under the
**Apache License, Version 2.0**. Copyright 2026 NeuroForgeIO and RKC
contributors. Redistributed
works must preserve applicable license and `NOTICE` terms. Third-party materials
retain their own licenses and ownership._
