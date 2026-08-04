import { describe, expect, it } from 'vitest'
import { settlementRevision } from './settlement'

describe('settlementRevision', () => {
  it('prefers the precision-safe committed revision', () => {
    expect(settlementRevision({ committed_revision: '90071992547409930', last_seq: 1 })).toBe('90071992547409930')
    expect(settlementRevision({ last_seq: 12 })).toBe('12')
  })

  it('does not trust an invalid preferred watermark', () => {
    expect(settlementRevision({ committed_revision: 'bad', last_seq: 12 })).toBeUndefined()
    expect(settlementRevision({})).toBeUndefined()
  })
})
