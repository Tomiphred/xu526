package cache

import (
	"crypto/sha256"
	"encoding/hex"
)

var sha256New = sha256.New
var hexEncode = hex.EncodeToString
