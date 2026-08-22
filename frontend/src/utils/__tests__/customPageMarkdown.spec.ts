import { describe, expect, it, vi } from 'vitest'

vi.mock('@/api/client', () => ({
  buildApiUrl: (path: string) => `/api/v1${path}`,
}))

import {
  buildPageImageUrl,
  isRelativeMarkdownAsset,
  renderCustomPageMarkdown,
  rewritePageImages,
} from '../customPageMarkdown'

describe('customPageMarkdown', () => {
  it('treats only plain relative paths as page assets', () => {
    expect(isRelativeMarkdownAsset('images/logo.png')).toBe(true)
    expect(isRelativeMarkdownAsset('./logo.png')).toBe(true)
    expect(isRelativeMarkdownAsset('logo.png?v=2#x')).toBe(true)
    expect(isRelativeMarkdownAsset('https://example.com/a.png')).toBe(false)
    expect(isRelativeMarkdownAsset('//cdn.example.com/a.png')).toBe(false)
    expect(isRelativeMarkdownAsset('/absolute.png')).toBe(false)
    expect(isRelativeMarkdownAsset('../escape.png')).toBe(false)
    expect(isRelativeMarkdownAsset('data:image/png;base64,AAAA')).toBe(false)
    expect(isRelativeMarkdownAsset('')).toBe(false)
  })

  it('builds encoded asset URLs under the page image endpoint', () => {
    expect(buildPageImageUrl('guide', 'images/my logo.png?v=1')).toBe(
      '/api/v1/pages/guide/images/images/my%20logo.png?v=1'
    )
  })

  it('rewrites relative images and leaves remote ones untouched', () => {
    const raw = '![a](images/a.png) ![b](https://x/b.png) ![c](../c.png)'
    expect(rewritePageImages('guide', raw)).toBe(
      '![a](/api/v1/pages/guide/images/images/a.png) ![b](https://x/b.png) ![c](../c.png)'
    )
  })

  it('renders sanitized HTML with heading ids and a TOC', () => {
    const { html, toc } = renderCustomPageMarkdown('guide', '# Hello World\n\n## 第二节\n\n<script>alert(1)</script>')
    expect(html).toContain('<h1 id="hello-world-0">Hello World</h1>')
    expect(html).toContain('<h2 id="第二节-1">第二节</h2>')
    expect(html).not.toContain('<script>')
    expect(toc).toEqual([
      { id: 'hello-world-0', text: 'Hello World', level: 1 },
      { id: '第二节-1', text: '第二节', level: 2 },
    ])
  })
})
