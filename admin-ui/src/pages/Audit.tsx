import { Alert, Table, Tag, Typography } from 'antd'
import { api } from '../api/client'
import type { AuditEntry } from '../api/types'
import { useLoad, when } from './hooks'

export function AuditPage() {
  const { data, error, loading } = useLoad(() => api.audit(200), [])

  return (
    <>
      <Typography.Paragraph type="secondary">
        Every operator action, including opening a conversation. Nothing in this service
        can edit or delete these rows.
      </Typography.Paragraph>
      {error && <Alert type="error" showIcon message={error} style={{ marginBottom: 12 }} />}
      <Table<AuditEntry>
        size="small"
        loading={loading}
        rowKey={(r) => r.at + r.actor + r.object + r.action}
        dataSource={data?.entries ?? []}
        scroll={{ x: 'max-content' }}
        pagination={{ pageSize: 25 }}
        columns={[
          { title: 'when', dataIndex: 'at', render: when },
          { title: 'actor', dataIndex: 'actor' },
          { title: 'action', dataIndex: 'action' },
          { title: 'object', dataIndex: 'object', className: 'mono' },
          {
            title: 'outcome', dataIndex: 'outcome',
            render: (o: string) => (
              <Tag color={o === 'ok' ? 'green' : o === 'conflict' ? 'gold' : 'red'}>{o}</Tag>
            ),
          },
          { title: 'detail', dataIndex: 'detail' },
        ]}
      />
    </>
  )
}
