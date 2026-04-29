package embed

import (
	_ "embed"
)

//go:embed config.ini
var Config []byte
