// Naming-compatible sync entry points. The projection itself is
// ProjectIndexStore; this subclass is only an API alias and introduces no
// second cache or reducer.
import { ProjectIndexStore } from './projectIndexStore'

export { ProjectIndexStore } from './projectIndexStore'
export type { ProjectIndexReadModel, ProjectIndexReadState } from './projectIndexStore'

export class ProjectIndexRepository extends ProjectIndexStore {}
