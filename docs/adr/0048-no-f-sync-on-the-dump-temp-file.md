# 0048. No `f.Sync()` on the dump temp file

**Status:** Accepted.

`dumpToFileWithEncryptor` used to fsync the temp file before `Stat`. The file lives on the worker pod's `emptyDir` and is deleted at the end of the run (`defer os.Remove`), so it never needs to survive a crash — fsync bought durability we explicitly don't want and added latency proportional to dump size. Reads (the fan-out re-opens the file, the verifier re-streams it) go through the page cache and see the written bytes regardless of whether they've hit disk, so correctness is unaffected. Removed.
