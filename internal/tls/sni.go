package tls

// ExtractSNI extracts the Server Name Indication (SNI) hostname from a TLS ClientHello
// Returns empty string if SNI is not found or data is invalid
func ExtractSNI(data []byte) string {
	// TLS handshake record format:
	// Byte 0: Content Type (0x16 for handshake)
	// Bytes 1-2: TLS version
	// Bytes 3-4: Length
	// Byte 5: Handshake Type (0x01 for ClientHello)
	// ...
	// SNI extension type: 0x0000

	if len(data) < 43 {
		return ""
	}

	// Check for TLS handshake (0x16) and ClientHello (0x01)
	if data[0] != 0x16 || data[5] != 0x01 {
		return ""
	}

	// Skip to extensions
	// Fixed headers: 43 bytes
	// Session ID length: 1 byte + variable
	// Cipher suites length: 2 bytes + variable
	// Compression methods length: 1 byte + variable
	pos := 43

	if pos >= len(data) {
		return ""
	}

	// Skip session ID
	sessionIDLen := int(data[pos])
	pos += 1 + sessionIDLen

	if pos+2 >= len(data) {
		return ""
	}

	// Skip cipher suites
	cipherSuitesLen := int(data[pos])<<8 | int(data[pos+1])
	pos += 2 + cipherSuitesLen

	if pos+1 >= len(data) {
		return ""
	}

	// Skip compression methods
	compressionMethodsLen := int(data[pos])
	pos += 1 + compressionMethodsLen

	if pos+2 >= len(data) {
		return ""
	}

	// Extensions length
	extensionsLen := int(data[pos])<<8 | int(data[pos+1])
	pos += 2

	if pos+extensionsLen > len(data) {
		return ""
	}

	// Parse extensions
	extensionsEnd := pos + extensionsLen
	for pos+4 <= extensionsEnd {
		extType := int(data[pos])<<8 | int(data[pos+1])
		extLen := int(data[pos+2])<<8 | int(data[pos+3])
		pos += 4

		if extType == 0x0000 { // SNI extension
			if pos+5 > len(data) {
				return ""
			}

			// SNI list length
			sniListLen := int(data[pos])<<8 | int(data[pos+1])
			pos += 2

			if sniListLen < 3 || pos+sniListLen > len(data) {
				return ""
			}

			// SNI type (0x00 for hostname)
			if data[pos] != 0x00 {
				return ""
			}
			pos++

			// Hostname length
			hostnameLen := int(data[pos])<<8 | int(data[pos+1])
			pos += 2

			if pos+hostnameLen > len(data) {
				return ""
			}

			// Extract hostname
			return string(data[pos : pos+hostnameLen])
		}

		pos += extLen
	}

	return ""
}
