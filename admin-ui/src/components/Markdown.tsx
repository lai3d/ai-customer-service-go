import type { ReactNode } from 'react'

// The model writes markdown -- **bold** and hyphen lists in almost every reply -- and a
// page that appends the reply as a plain string shows an operator asterisks where the
// customer saw bold. That was a real defect in the page this application replaces, found
// by rendering it in a browser and invisible to every test, because the text arrived
// correctly and only the drawing was wrong.
//
// A deliberately small subset, built as React elements. No dangerouslySetInnerHTML
// anywhere in this application, so nothing the model writes is ever parsed as markup --
// which matters more here than in an ordinary chat client, because the model's input
// includes retrieved passages, the injection path a system prompt can only ask about.
//
// Links are absent on purpose. Every other construct here is formatting; a link is a
// capability, and `[text](javascript:...)` or a model-authored href pointing anywhere is
// the one piece of markdown that does something rather than looks like something.
export function Markdown({ text }: { text: string }): ReactNode {
  const blocks: ReactNode[] = []
  let list: ReactNode[] = []
  let key = 0

  const flush = () => {
    if (list.length) {
      blocks.push(<ul key={key++}>{list}</ul>)
      list = []
    }
  }

  for (const line of text.split('\n')) {
    const item = /^\s*[-*]\s+(.*)$/.exec(line)
    if (item) {
      list.push(<li key={key++}>{inline(item[1] ?? '')}</li>)
      continue
    }
    flush()
    if (line.trim() === '') continue
    blocks.push(<p key={key++}>{inline(line)}</p>)
  }
  flush()
  return <>{blocks}</>
}

/** Handles **bold** and `code`; everything else becomes a text node. */
export function inline(text: string): ReactNode[] {
  const pattern = /\*\*([^*]+)\*\*|`([^`]+)`/g
  const out: ReactNode[] = []
  let last = 0
  let match: RegExpExecArray | null
  let key = 0
  while ((match = pattern.exec(text)) !== null) {
    if (match.index > last) out.push(text.slice(last, match.index))
    if (match[1] !== undefined) out.push(<strong key={key++}>{match[1]}</strong>)
    else out.push(<code key={key++}>{match[2]}</code>)
    last = pattern.lastIndex
  }
  if (last < text.length) out.push(text.slice(last))
  return out
}
