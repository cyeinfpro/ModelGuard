# Fingerprint reference artifacts

From Ikaleio/lm-detector, MIT copyright xqy2006. Artifacts pin upstream data
downloaded for local inference, not an online scoring API. Original project:
https://github.com/Ikaleio/lm-detector

Upstream commit: a4ef212df06817e8071acd05f00219f4c9eb2f9a
Bank built at: 2026-10-27T05:02:50.758637+00:00
Models: 64; includes gpt-6.1-terra as its own reference identity.

shared_detector.bin SHA-256: 9ac93e5e580f5bac887f97a20bfae15d0a3ad08c6372e4b3552fce6708078a80
model_metadata.json SHA-256: b121009f6c01848dcb7175c6d11a5b46002c99a978aa6dfcb3312db4d004317f

The detector bytes are copied unchanged from upstream data/shared_detector.bin.
Metadata is projected from the matching data/unified_bank.json, retaining model
order, labels, family and response counts. No local evaluation or account
responses are enrolled into the reference bank.

The 20 parity fixtures retain the previous 16 public answer groups and add
three complete public reference answers for each of the four new identities:
gpt-6.1-terra, gemini-3.5-flash-lite, kimi-k3.1-pro, minimax-m3.1-flash.
These fixtures verify implementation parity, not model identification accuracy.
The fixture results were generated with the pinned upstream JavaScript scorer;
the offline parity tests verify the bundled Go implementation against them.

Ranking calibration and quarantine decision thresholds are unchanged.
