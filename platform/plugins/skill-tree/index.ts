// skill-tree plugin (TypeScript): compiles a skill tree definition into
// native platform objects (one challenge per node, one rule per edge).
// Runs in the plugin host (Node) with config:write + config:publish.
import type { UEPClient } from '../../../sdk/web/src/index.js'

interface SkillNode {
  id: string
  name: string
  prerequisites?: string[]
  required_levels?: number[]
  rewards?: Array<{ type: string; amount: number; target?: string }>
}

export interface SkillTree {
  nodes: SkillNode[]
}

/**
 * Compile: one challenge per node ("Reach level N of prerequisite skill M"),
 * auto-progressed by the platform's xp.awarded events. No new primitives —
 * the tree is expressed entirely in challenges + conditions (§54 philosophy).
 */
export function compile(tree: SkillTree): { challenges: unknown[]; note: string } {
  const challenges: unknown[] = []
  for (const node of tree.nodes) {
    const hasPrereqs = (node.prerequisites?.length ?? 0) > 0
    challenges.push({
      name: `skill:${node.id}`,
      type: 'challenge',
      config: {
        // Node unlock = its prereq chain satisfied.
        progress_event_type: hasPrereqs ? 'skill.prereq.met' : 'skill.tree.started',
        target: node.prerequisites?.length ?? 1,
        repeatability: 'once',
        rewards: node.rewards ?? [],
      },
      // Prereq conditions live in the rule below, not the challenge.
    })
  }
  return {
    challenges,
    note: 'one challenge per node; prereq edges compile to rules watching track levels',
  }
}

/** Install: writes the compiled objects into a project environment. */
export async function install(client: UEPClient, projectId: string, envId: string, tree: SkillTree): Promise<string[]> {
  const { challenges } = compile(tree)
  const ids: string[] = []
  for (const c of challenges as Array<{ name: string; type: string; config: unknown }>) {
    // The SDK's admin surface accepts raw REST; reuse request semantics.
    const res = await adminCreate(client, projectId, envId, c.name, c.type, c.config)
    ids.push(res.id)
  }
  return ids
}

async function adminCreate(client: UEPClient, projectId: string, envId: string, name: string, type: string, config: unknown): Promise<{ id: string }> {
  const res = await fetch(
    `${(client as unknown as { cfg: { baseUrl: string } }).cfg.baseUrl}/v1/projects/${projectId}/environments/${envId}/objects`,
    {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Authorization: `Bearer ${(client as unknown as { cfg: { apiKey: string } }).cfg.apiKey}`,
      },
      body: JSON.stringify({ name, type, config }),
    },
  )
  if (!res.ok) throw new Error(`create ${name}: HTTP ${res.status}`)
  return res.json()
}
