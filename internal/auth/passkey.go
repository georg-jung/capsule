package auth

import (
	"encoding/hex"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

var knownPasskeyProviders = map[string]string{
	"ea9b8d664d011d213ce4b6b48cb575d4": "Google Password Manager",
	"08987058cadc4b81b6e130de50dcbe96": "Windows Hello",
	"9ddd1817af5a4672a2b93e3dd95000a9": "Windows Hello",
}

func PasskeyName(credential *webauthn.Credential) string {
	if name, found := knownPasskeyProviders[hex.EncodeToString(credential.Authenticator.AAGUID)]; found {
		return name
	}
	if credential.Authenticator.Attachment == protocol.Platform {
		if credential.Flags.BackupEligible {
			return "Synced platform passkey"
		}
		return "Platform passkey"
	}
	for _, transport := range credential.Transport {
		if transport == protocol.USB || transport == protocol.NFC || transport == protocol.BLE || transport == protocol.SmartCard {
			return "Security key"
		}
	}
	return "Passkey"
}

func FormatAAGUID(aaguid []byte) string {
	if len(aaguid) != 16 {
		return hex.EncodeToString(aaguid)
	}
	raw := hex.EncodeToString(aaguid)
	return raw[:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:]
}
