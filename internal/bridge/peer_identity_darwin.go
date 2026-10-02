package bridge

import "errors"

func verifyPlatformPeerIdentity(_ int, _, _ string) error {
	return errors.New("unsupported peer process identity format")
}
