// Random ID generation that also works outside secure contexts.
// `crypto.randomUUID` is only exposed in secure contexts, so pages served over
// plain HTTP on a LAN address cannot use it; `crypto.getRandomValues` remains
// available there and is used as the fallback source of randomness.

function formatUUID(bytes: Uint8Array): string {
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	const hex = [...bytes].map((byte) => byte.toString(16).padStart(2, '0')).join('')
	return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
}

/** Returns an RFC 4122 version 4 UUID, preferring the strongest available source. */
export function randomID(): string {
	const cryptoObject = globalThis.crypto
	if (cryptoObject) {
		try {
			if (typeof cryptoObject.randomUUID === 'function') return cryptoObject.randomUUID()
		} catch {
			// Fall through to getRandomValues when randomUUID is unavailable or fails.
		}
		try {
			if (typeof cryptoObject.getRandomValues === 'function') return formatUUID(cryptoObject.getRandomValues(new Uint8Array(16)))
		} catch {
			// Fall through to the last-resort generator below.
		}
	}
	const bytes = new Uint8Array(16)
	for (let index = 0; index < bytes.length; index += 1) bytes[index] = Math.floor(Math.random() * 256)
	return formatUUID(bytes)
}
