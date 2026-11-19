# Safe mode recognition model

`rec.onnx` is PaddleOCR's PP-OCRv5 English/Latin text recogniser, converted to ONNX
(https://huggingface.co/monkt/paddleocr-onnx, `languages/english/rec.onnx`,
SHA-256 `4e16deb22c4da6468bdca539b2cd3c8687825538b67109177c47d359ab994cd7`).
`dict.txt` is its character list. Both are Apache License 2.0
(PaddlePaddle authors). They run locally in the sender page; nothing is uploaded.

The ONNX Runtime Web files under `ort/` are copied from `node_modules` at build
time (MIT, Microsoft).
