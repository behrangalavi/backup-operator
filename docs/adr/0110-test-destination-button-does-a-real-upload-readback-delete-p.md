# 0110. Test-destination button does a real Upload+Readback+Delete probe

**Status:** Accepted.

The original test handler did `dial → List(<nonexistent-path>)`. For SFTP this verified the SSH handshake; for FTPS my own `walkList` swallowed "directory does not exist" as empty. Result: a destination where the user could log in but had no write permission still showed green on the Test button — exactly the failure mode that produces "backup runs forever, nothing lands on the destination" with no diagnostic. New flow: upload a ~30-byte probe object with a random suffix, read it back, verify byte equality, delete it. Any failure surfaces with a clear message (`upload failed: 550 Permission denied`, `readback mismatch`, etc.). Best-effort cleanup via `defer` in a fresh context so even Delete-fails leave the destination provably writable.
