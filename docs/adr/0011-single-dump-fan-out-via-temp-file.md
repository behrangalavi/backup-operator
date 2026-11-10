# 0011. Single dump → fan-out via temp file

**Status:** Accepted.

Streaming the encrypted dump to N destinations simultaneously means the slowest destination throttles the dump phase. Materialising once locally costs `emptyDir` space but decouples destinations.
