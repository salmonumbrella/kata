// A session locator is safe to open when it is an absolute HTTP(S) URL without
// credentials, query or fragment. Anything else stays readable text only.
export function safeSessionURL(value: unknown): string | undefined {
  if (typeof value !== 'string') return undefined
  try {
    const url = new URL(value)
    if (
      (url.protocol === 'http:' || url.protocol === 'https:') &&
      !url.username &&
      !url.password &&
      !value.includes('?') &&
      !value.includes('#')
    ) {
      return value
    }
  } catch {
    // A valid identifier remains useful without a usable locator.
  }
  return undefined
}
