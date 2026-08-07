import { CodexLoginStore } from './codexLoginStore'
export { CodexLoginStore } from './codexLoginStore'
export type { CodexLoginReadError, CodexLoginReadModel, CodexLoginReadState, CodexLoginSource, CodexLoginDomain } from '../repositories/codexLogin'

// Naming-compatible sync entry point. The store is the only local projection.
export class CodexLoginRepository extends CodexLoginStore {}

export function selectCodexLogin(repository: CodexLoginRepository, provider: string) { return repository.getSnapshot(provider) }
