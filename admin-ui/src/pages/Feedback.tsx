import { useState } from 'react'
import { Alert, Button, Card, Space, Switch, Tag, Typography } from 'antd'
import { api, ApiError } from '../api/client'
import type { FeedbackItem } from '../api/types'
import { Markdown } from '../components/Markdown'
import { useLoad, when } from './hooks'

// The queue, not the widget.
//
// A rating that nobody reads is a suggestion box. What makes this worth collecting is that
// the service already knows what it said, what it retrieved and what it cost — so a
// reported-wrong answer arrives as a specified piece of work rather than a complaint, and
// clearing it means somebody wrote the eval case or fixed the entry.
export function FeedbackPage({ canWrite }: { canWrite: boolean }) {
  const [includeHandled, setIncludeHandled] = useState(false)
  const { data, error, loading, reload } = useLoad(
    () => api.feedback(includeHandled), [includeHandled])
  const [problem, setProblem] = useState('')

  const items = data?.items ?? []

  return (
    <>
      {error && <Alert type="error" showIcon message={error} style={{ marginBottom: 12 }} />}
      {problem && <Alert type="error" showIcon message={problem} style={{ marginBottom: 12 }} />}

      <Space style={{ marginBottom: 12 }}>
        <Typography.Text type="secondary">
          {items.filter((i) => !i.handled).length} open
        </Typography.Text>
        <Switch
          size="small"
          checked={includeHandled}
          onChange={setIncludeHandled}
          checkedChildren="with cleared"
          unCheckedChildren="open only"
        />
      </Space>

      {!loading && items.length === 0 && (
        <Alert
          type="success"
          showIcon
          message="Nothing reported wrong."
          description="Either the answers are good or nobody is telling you. The customer rating on the demo page and the Judge action in a conversation are the two ways in."
        />
      )}

      {items.map((item) => (
        <FeedbackCard
          key={item.turnId + item.source}
          item={item}
          canWrite={canWrite}
          onCleared={() => void reload()}
          onProblem={setProblem}
        />
      ))}
    </>
  )
}

function FeedbackCard(props: {
  item: FeedbackItem
  canWrite: boolean
  onCleared: () => void
  onProblem: (message: string) => void
}) {
  const [clearing, setClearing] = useState(false)
  const { item } = props

  return (
    <Card size="small" style={{ marginBottom: 10 }}>
      <Space wrap size="middle" style={{ marginBottom: 8 }}>
        <Tag color={item.verdict === 'wrong' ? 'red' : 'gold'}>{item.verdict}</Tag>
        {/* Which source said it is the point: a customer knows whether they were helped
            and nothing about correctness; an operator knows the opposite. */}
        <Tag>{item.source}</Tag>
        <Typography.Text type="secondary">{when(item.at)} · {item.actor}</Typography.Text>
        {item.model && <span className="mono">{item.model}</span>}
        {item.handled && <Tag color="green">cleared</Tag>}
      </Space>

      {item.note && (
        <Alert type="info" style={{ marginBottom: 8 }} message={item.note} />
      )}

      <p className="turn-question">{item.question}</p>
      {item.answer
        ? <div className="turn-reply"><Markdown text={item.answer} /></div>
        : <Typography.Text type="secondary">No reply was recorded.</Typography.Text>}

      {item.entries && item.entries.length > 0 && (
        <Typography.Paragraph type="secondary" style={{ marginTop: 10, fontSize: 12 }}>
          answered from: {item.entries.join(', ')}
        </Typography.Paragraph>
      )}

      {props.canWrite && !item.handled && (
        <Button
          size="small"
          loading={clearing}
          onClick={async () => {
            setClearing(true)
            try {
              await api.clearFeedback(item.turnId, item.source)
              props.onCleared()
            } catch (err) {
              props.onProblem(err instanceof ApiError ? err.message : String(err))
            } finally {
              setClearing(false)
            }
          }}
        >
          Mark acted on
        </Button>
      )}
    </Card>
  )
}
