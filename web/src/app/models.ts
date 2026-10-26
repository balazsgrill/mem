export interface BucketSummary {
  bucket: string;
  active_count: number;
  core_count: number;
  semantic_count: number;
  total_count: number;
}

export interface MemoryDetail {
  id: number;
  bucket: string;
  tier: 'core' | 'semantic' | string;
  subject: string;
  content: string;
  metadata?: Record<string, any>;
  superseded_by?: number | null;
  valid_from: string;
  valid_until?: string | null;
}

export interface AgentToken {
  token: string;
  agent_id: string;
  description?: string;
  read_buckets: string[];
  write_buckets: string[];
  revoked: boolean;
  created_at: string;
  updated_at: string;
}

export interface AgentTokenUpdate {
  agent_id?: string;
  description?: string;
  read_buckets?: string[];
  write_buckets?: string[];
  revoked?: boolean;
}
