# 0137. Redis empty-dump floor scales with the key count

**Status:** Accepted.

The flat 200-byte raw floor false-failed legitimate 5–10-key keysets (~120–160 B). The floor is now `18 + 4·keys`: the minimal RDB framing (9-byte magic/version + EOF + 8-byte CRC64) plus the physically smallest encodable key entry (type opcode, 1-byte length, 1-byte key, 1-byte empty value). Real dumps are ~5× that per key, so the floor never exceeds a legitimate dump; the accepted trade-off is that keysets under ~20 keys can let a header-only dump through — false hard-fails were judged worse (the project's standing heuristic-threshold principle).
