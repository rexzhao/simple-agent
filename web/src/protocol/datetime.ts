const strictRFC3339Pattern = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(Z|[+-](\d{2}):(\d{2}))$/

export function isRFC3339Timestamp(value: unknown): value is string {
  if (typeof value !== 'string') return false
  const matches = strictRFC3339Pattern.exec(value)
  if (matches === null) return false

  const year = BigInt(matches[1])
  const month = BigInt(matches[2])
  const day = BigInt(matches[3])
  const hour = BigInt(matches[4])
  const minute = BigInt(matches[5])
  const second = BigInt(matches[6])
  if (
    month < 1n || month > 12n ||
    day < 1n || day > daysInMonth(year, month) ||
    hour > 23n || minute > 59n || second > 59n
  ) return false

  if (matches[8] !== 'Z') {
    const offsetHour = BigInt(matches[9])
    const offsetMinute = BigInt(matches[10])
    if (offsetHour > 23n || offsetMinute > 59n) return false
  }
  return true
}

function daysInMonth(year: bigint, month: bigint): bigint {
  if (month === 2n) return isLeapYear(year) ? 29n : 28n
  if (month === 4n || month === 6n || month === 9n || month === 11n) return 30n
  return 31n
}

function isLeapYear(year: bigint): boolean {
  return year % 400n === 0n || (year % 4n === 0n && year % 100n !== 0n)
}
