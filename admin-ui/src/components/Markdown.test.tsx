import { describe, expect, it } from 'vitest'
import { renderToStaticMarkup } from 'react-dom/server'
import { Markdown } from './Markdown'

const html = (text: string) => renderToStaticMarkup(<Markdown text={text} />)

describe('the reply renderer', () => {
  it('renders the bold the customer saw', () => {
    // The defect this exists for: an operator was shown **TKT-4700** as literal
    // asterisks while the customer had seen it in bold.
    expect(html('your ticket is **TKT-4700** now')).toContain('<strong>TKT-4700</strong>')
  })

  it('renders hyphen lists and inline code', () => {
    expect(html('- one\n- two')).toBe('<ul><li>one</li><li>two</li></ul>')
    expect(html('run `make deps`')).toContain('<code>make deps</code>')
  })

  it('never turns the model\'s text into markup', () => {
    // The text being rendered was written by a model whose own input includes retrieved
    // passages, which is the injection path a system prompt can only ask about.
    const out = html('<script>alert(1)</script> and <img src=x onerror=alert(2)>')
    expect(out).not.toContain('<script>')
    expect(out).not.toContain('<img')
    expect(out).toContain('&lt;script&gt;')
  })

  it('does not build links, because a link is a capability', () => {
    const out = html('[click me](javascript:alert(1)) and [there](https://evil.test)')
    expect(out).not.toContain('<a ')
    expect(out).toContain('click me')
  })

  it('keeps a blank line from becoming an empty paragraph', () => {
    expect(html('one\n\ntwo')).toBe('<p>one</p><p>two</p>')
  })
})
