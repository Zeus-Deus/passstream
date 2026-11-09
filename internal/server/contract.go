package server

import _ "embed"

//go:generate python3 ../../scripts/generate_llms.py
//go:embed llms.txt
var contract string
