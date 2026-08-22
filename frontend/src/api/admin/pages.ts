/**
 * Admin custom pages API.
 *
 * Pages (Markdown) and their assets live in PostgreSQL so every replica serves
 * the same content (issue #7). Limits are declared by the backend and returned
 * with the list call; the UI never hardcodes its own copy.
 */

import { apiClient } from '../client'

export interface CustomPageSummary {
  slug: string
  content_size: number
  asset_count: number
  asset_bytes: number
  updated_by?: number
  created_at: string
  updated_at: string
}

export interface CustomPage {
  slug: string
  content: string
  content_size: number
  updated_by?: number
  created_at: string
  updated_at: string
}

export interface CustomPageAsset {
  path: string
  content_type: string
  size: number
  created_at: string
  updated_at: string
}

export interface CustomPageLimits {
  max_content_size: number
  max_asset_size: number
  max_assets: number
  max_slug_length: number
}

export interface CustomPageListResponse {
  pages: CustomPageSummary[]
  limits: CustomPageLimits
}

function encodeAssetPath(path: string): string {
  return path
    .split('/')
    .filter((part) => part !== '')
    .map((part) => encodeURIComponent(part))
    .join('/')
}

export async function list(): Promise<CustomPageListResponse> {
  const { data } = await apiClient.get<CustomPageListResponse>('/admin/pages')
  return data
}

export async function get(slug: string): Promise<CustomPage> {
  const { data } = await apiClient.get<CustomPage>(`/admin/pages/${encodeURIComponent(slug)}`)
  return data
}

export async function save(slug: string, content: string): Promise<CustomPage> {
  const { data } = await apiClient.put<CustomPage>(`/admin/pages/${encodeURIComponent(slug)}`, { content })
  return data
}

export async function remove(slug: string): Promise<{ message: string }> {
  const { data } = await apiClient.delete<{ message: string }>(`/admin/pages/${encodeURIComponent(slug)}`)
  return data
}

export async function listAssets(slug: string): Promise<CustomPageAsset[]> {
  const { data } = await apiClient.get<CustomPageAsset[]>(`/admin/pages/${encodeURIComponent(slug)}/assets`)
  return data
}

/**
 * Upload (or overwrite) one asset. The file is sent as a raw body with its own
 * Content-Type; the backend falls back to the extension when the browser
 * reports none.
 */
export async function uploadAsset(slug: string, path: string, file: Blob): Promise<CustomPageAsset> {
  const { data } = await apiClient.put<CustomPageAsset>(
    `/admin/pages/${encodeURIComponent(slug)}/assets/${encodeAssetPath(path)}`,
    file,
    { headers: { 'Content-Type': file.type || 'application/octet-stream' } }
  )
  return data
}

export async function deleteAsset(slug: string, path: string): Promise<{ message: string }> {
  const { data } = await apiClient.delete<{ message: string }>(
    `/admin/pages/${encodeURIComponent(slug)}/assets/${encodeAssetPath(path)}`
  )
  return data
}

export const pagesAPI = {
  list,
  get,
  save,
  remove,
  listAssets,
  uploadAsset,
  deleteAsset
}

export default pagesAPI
