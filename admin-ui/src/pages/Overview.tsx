import { useState } from 'react'
import { Alert, Card, Col, Row, Select, Space, Statistic, Table, Typography } from 'antd'
import { api } from '../api/client'
import { useLoad } from './hooks'

// The server accepts anything from an hour to ninety days. The page offered one window.
const WINDOWS = [
  { value: 24, label: 'last 24 hours' },
  { value: 168, label: 'last 7 days' },
  { value: 720, label: 'last 30 days' },
  { value: 2160, label: 'last 90 days' },
]

export function OverviewPage() {
  const [hours, setHours] = useState(168)
  const { data, error, loading } = useLoad(() => api.overview(hours), [hours])

  if (error) return <Alert type="error" showIcon message={error} />

  const turns = Object.values(data?.turnsByOutcome ?? {}).reduce((a, b) => a + b, 0)
  const completed = data?.turnsByOutcome?.['completed'] ?? 0

  return (
    <>
      <Space style={{ marginBottom: 12 }}>
        <Select style={{ width: 180 }} value={hours} onChange={setHours} options={WINDOWS} />
        {data && (
          <Typography.Text type="secondary">
            since {new Date(data.since).toLocaleString()}
          </Typography.Text>
        )}
      </Space>
      <Row gutter={[12, 12]}>
        {[
          { title: 'turns', value: turns },
          { title: 'conversations', value: data?.conversations ?? 0 },
          { title: 'completed', value: completed },
          { title: 'not completed', value: turns - completed },
          { title: 'tokens in', value: data?.inputTokens ?? 0 },
          { title: 'tokens out', value: data?.outputTokens ?? 0 },
        ].map((c) => (
          <Col key={c.title} xs={12} sm={8} lg={4}>
            <Card size="small" loading={loading}>
              <Statistic title={c.title} value={c.value} />
            </Card>
          </Col>
        ))}
        <Col xs={12} sm={8} lg={4}>
          <Card size="small" loading={loading}>
            <Statistic
              title="estimated cost"
              value={data?.costUsd ?? 0}
              precision={4}
              prefix="$"
            />
          </Card>
        </Col>
      </Row>

      {/* A flat cost meter is indistinguishable from a cheap week unless the page says
          which turns it could not price. */}
      {data && data.unpricedTurns > 0 && (
        <Alert
          type="warning"
          showIcon
          style={{ marginTop: 12 }}
          message={`${data.unpricedTurns} turn(s) ran on a model with no price entry, so the cost above is incomplete.`}
        />
      )}

      <Row gutter={[12, 12]} style={{ marginTop: 12 }}>
        <Col xs={24} lg={12}>
          <Card size="small" title="Turn outcomes" loading={loading}>
            <Table
              size="small"
              pagination={false}
              rowKey="outcome"
              dataSource={Object.entries(data?.turnsByOutcome ?? {}).map(([outcome, n]) => ({ outcome, n }))}
              columns={[
                { title: 'outcome', dataIndex: 'outcome' },
                { title: 'turns', dataIndex: 'n', align: 'right' },
              ]}
            />
          </Card>
        </Col>
        <Col xs={24} lg={12}>
          <Card size="small" title="Tickets" loading={loading}>
            <Table
              size="small"
              pagination={false}
              rowKey="state"
              dataSource={Object.entries(data?.tickets ?? {}).map(([state, n]) => ({ state, n }))}
              columns={[
                { title: 'state', dataIndex: 'state' },
                { title: 'tickets', dataIndex: 'n', align: 'right' },
              ]}
            />
          </Card>
        </Col>
      </Row>
    </>
  )
}
