/**
 * Public Intelligence Check API endpoints
 * 「智力检测」作品墙：所有登录用户可见，后端已脱敏（不含账号名 / 上游标识 / 模型名）。
 */

import { apiClient } from './client'

/** 脱敏后的作品墙卡片。 */
export interface IntelligenceCheckPublicCard {
  /** 展示序号，从 1 开始 */
  index: number
  status: string
  verdict: string
  latency_ms: number
  error_code?: string
  has_artifact: boolean
  artifact_url?: string
  created_at: string
}

export interface IntelligenceCheckPublicWall {
  items: IntelligenceCheckPublicCard[]
  total: number
}

/**
 * 取脱敏作品墙：每个参与过跑测的账号一张卡片，按展示序号升序。
 */
export async function listPublicRuns(): Promise<IntelligenceCheckPublicWall> {
  const { data } = await apiClient.get<IntelligenceCheckPublicWall>('/intelligence-check/runs')
  return data
}

/**
 * 取作品原文。
 * 后端以 text/plain 返回且带禁止脚本的 CSP，这里必须显式声明 responseType 并禁用
 * axios 的默认 JSON 解析，否则 HTML 会被当成 JSON 解析失败。
 * @param id - 跑测记录 ID（从卡片的 artifact_url 中解析）
 */
export async function getPublicArtifact(id: number): Promise<string> {
  const { data } = await apiClient.get<string>(`/intelligence-check/runs/${id}/artifact`, {
    responseType: 'text',
    transformResponse: [(value: string) => value]
  })
  return typeof data === 'string' ? data : ''
}

export const intelligenceCheckPublicAPI = {
  listPublicRuns,
  getPublicArtifact
}

export default intelligenceCheckPublicAPI
