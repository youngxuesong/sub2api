const FALLBACK_LABEL_MAX_LENGTH = 17
const SEMANTIC_VERSION_PATTERN = /(?:^|[^0-9])v?(\d+\.\d+\.\d+)(?=$|[^0-9])/i

export function formatVersionLabel(value?: string | null): string {
  const version = value?.trim()
  if (!version) return ''

  const semanticVersion = version.match(SEMANTIC_VERSION_PATTERN)?.[1]
  if (semanticVersion) return `v${semanticVersion}`

  if (version.length <= FALLBACK_LABEL_MAX_LENGTH) return version
  return `${version.slice(0, FALLBACK_LABEL_MAX_LENGTH - 3)}...`
}
