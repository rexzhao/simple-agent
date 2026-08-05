import { describe, expect, it } from 'vitest'
import { createCurrentProjectSignal, SessionIndexInterestPolicy } from './interestPolicy'

describe('SessionIndexInterestPolicy', () => {
  it('keeps exactly the signal-selected resource subscribed independently of UI listeners', () => {
    const signal = createCurrentProjectSignal('project_a')
    const subscribed: string[] = []
    const released: string[] = []
    const runtime = {
      subscribe: (resource: { type: 'session_index'; id: string }) => {
        subscribed.push(resource.id)
        return () => released.push(resource.id)
      },
    }
    const policy = new SessionIndexInterestPolicy(runtime, signal)
    policy.start()
    expect(policy.projectID).toBe('project_a')
    expect(subscribed).toEqual(['project_a'])
    // No React component subscription is involved; policy ownership is the
    // durable interest reference that remains after page unmount.
    signal.set('project_b')
    expect(released).toEqual(['project_a'])
    expect(subscribed).toEqual(['project_a', 'project_b'])
    expect(policy.projectID).toBe('project_b')
  })

  it('makes repeated start/stop and null projects leak-free', () => {
    const signal = createCurrentProjectSignal(null)
    let active = 0
    let subscribeCalls = 0
    let releaseCalls = 0
    const runtime = {
      subscribe: () => {
        active += 1
        subscribeCalls += 1
        return () => { active -= 1; releaseCalls += 1 }
      },
    }
    const policy = new SessionIndexInterestPolicy(runtime, signal)
    policy.start()
    policy.start()
    expect(active).toBe(0)
    signal.set('project_a')
    signal.set('project_a')
    expect(active).toBe(1)
    policy.stop()
    policy.stop()
    expect(active).toBe(0)
    expect(subscribeCalls).toBe(1)
    expect(releaseCalls).toBe(1)
  })

  it('does not replace a subscription when the signal value is unchanged', () => {
    const signal = createCurrentProjectSignal('project_a')
    let calls = 0
    const policy = new SessionIndexInterestPolicy({
      subscribe: () => { calls += 1; return () => {} },
    }, signal)
    policy.start()
    signal.set('project_a')
    expect(calls).toBe(1)
    policy.stop()
  })

  it('passes explicit retain/evict policy to a bounded runtime', () => {
    const signal = createCurrentProjectSignal('project_a')
    const options: boolean[] = []
    const evicted: string[] = []
    const runtime = {
      subscribe: (_resource: { type: 'session_index'; id: string }, value?: { retainOnRelease?: boolean }) => {
        options.push(value?.retainOnRelease === true)
        return () => {}
      },
      evict: (resource: { type: 'session_index'; id: string }) => evicted.push(resource.id),
    }
    const policy = new SessionIndexInterestPolicy(runtime, signal, { retainReleased: true })
    policy.start()
    signal.set('project_b')
    policy.stop()
    expect(options).toEqual([true, true])
    expect(evicted).toEqual([])

    const evicting = new SessionIndexInterestPolicy(runtime, createCurrentProjectSignal('project_c'))
    evicting.start()
    evicting.stop()
    expect(evicted).toEqual(['project_c'])
  })
})
