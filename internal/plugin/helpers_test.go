package plugin

import "os"

func osWrite(path, body string) error { return os.WriteFile(path, []byte(body), 0o600) }
