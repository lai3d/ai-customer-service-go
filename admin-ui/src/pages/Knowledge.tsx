import { useState } from 'react'
import {
  Alert, Button, Form, Input, Modal, Popconfirm, Space, Table, Tag, Typography,
} from 'antd'
import { api, ApiError } from '../api/client'
import type { CorpusVersion, KnowledgeEntry } from '../api/types'
import { MAX_ANSWER_LENGTH } from '../api/types'
import { useLoad, when } from './hooks'

// The editor and the publication are two different actions on this page, and keeping them
// visibly separate is the design rather than the layout. An edit changes a draft; a
// publication changes what customers are told. A page where saving and publishing look
// alike is a page where somebody publishes a half-written sentence by reflex.
export function KnowledgePage({ canWrite }: { canWrite: boolean }) {
  const { data, error, loading, reload } = useLoad(() => api.knowledge(), [])
  const versions = useLoad(() => api.corpusVersions(), [])
  const [editing, setEditing] = useState<KnowledgeEntry | null>(null)
  const [publishing, setPublishing] = useState(false)
  const [note, setNote] = useState('')
  const [problem, setProblem] = useState('')

  const entries = data?.entries ?? []
  const live = entries.filter((e) => !e.deleted).length

  const publish = async () => {
    if (!data) return
    setPublishing(true)
    setProblem('')
    try {
      const { version } = await api.publish(note.trim(), data.revision)
      setNote('')
      Modal.success({
        title: 'Published',
        content: `Customers are now answered from ${version}.`,
      })
      void reload()
      void versions.reload()
    } catch (err) {
      // A 409 is the ordinary case in a team, not a fault: somebody else published while
      // this page was open. Saying which is the difference between reloading and guessing.
      setProblem(err instanceof ApiError ? err.message : String(err))
      void reload()
    } finally {
      setPublishing(false)
    }
  }

  return (
    <>
      {error && <Alert type="error" showIcon message={error} style={{ marginBottom: 12 }} />}
      {problem && <Alert type="error" showIcon message={problem} style={{ marginBottom: 12 }} />}

      {/* The drafts and the live corpus can disagree, and a rollback is the case where an
          operator would never guess: it changes what customers are told and leaves the
          editor showing something else. Found by opening this page after one. */}
      {data?.unpublishedChanges && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 12 }}
          message="These drafts are not what customers are being answered from."
          description="Something has been edited, or the active version was rolled back, since the live version was published. Publish to make the list below live."
        />
      )}

      <Space style={{ marginBottom: 12 }} wrap>
        <Typography.Text type="secondary">
          {live} live {live === 1 ? 'entry' : 'entries'}
          {data?.activeVersion
            ? <> · customers are answered from <span className="mono">{data.activeVersion}</span></>
            : <> · <b>no version is active</b>; nothing is being retrieved</>}
        </Typography.Text>
        {canWrite && (
          <>
            <Button type="primary" onClick={() => setEditing({
              entryId: '', language: 'en', category: '', question: '', answer: '',
              deleted: false, updatedAt: '', updatedBy: '',
            })}>New entry</Button>
            <Input
              style={{ width: 260 }}
              placeholder="what changed, for the version history"
              value={note}
              onChange={(e) => setNote(e.target.value)}
            />
            <Popconfirm
              title="Publish to customers?"
              description="Every live entry becomes a new corpus version and customers are answered from it immediately."
              okText="Publish"
              onConfirm={publish}
            >
              <Button danger loading={publishing}>Publish</Button>
            </Popconfirm>
          </>
        )}
      </Space>

      <Table<KnowledgeEntry>
        size="small"
        loading={loading}
        rowKey={(e) => e.entryId + ':' + e.language}
        dataSource={entries}
        scroll={{ x: 'max-content' }}
        pagination={{ pageSize: 20, showTotal: (n) => `${n} draft(s)` }}
        columns={[
          { title: 'entry', dataIndex: 'entryId', className: 'mono' },
          { title: 'lang', dataIndex: 'language', width: 70 },
          { title: 'category', dataIndex: 'category' },
          { title: 'question', dataIndex: 'question', width: 340 },
          {
            title: '', dataIndex: 'deleted', width: 90,
            render: (deleted: boolean) =>
              deleted ? <Tag color="red">deleted</Tag> : null,
          },
          { title: 'changed', dataIndex: 'updatedAt', render: when },
          { title: 'by', dataIndex: 'updatedBy' },
          ...(canWrite ? [{
            title: '', key: 'actions', width: 150,
            render: (_: unknown, e: KnowledgeEntry) => (
              <Space>
                <Button size="small" onClick={() => setEditing(e)}>Edit</Button>
                {!e.deleted && (
                  <Popconfirm
                    title="Delete this entry?"
                    description="It stops being retrievable at the next publication, not now."
                    onConfirm={async () => {
                      try {
                        await api.deleteEntry(e.entryId, e.language)
                        void reload()
                      } catch (err) {
                        setProblem(err instanceof ApiError ? err.message : String(err))
                      }
                    }}
                  >
                    <Button size="small" danger>Delete</Button>
                  </Popconfirm>
                )}
              </Space>
            ),
          }] : []),
        ]}
      />

      <Typography.Title level={5} style={{ marginTop: 20 }}>Versions</Typography.Title>
      <Typography.Paragraph type="secondary">
        Each publication is a version. Rolling back re-activates one — and a version whose
        documents have been swept by retention is refused rather than activated empty.
      </Typography.Paragraph>
      <Table<CorpusVersion>
        size="small"
        loading={versions.loading}
        rowKey="version"
        dataSource={versions.data?.versions ?? []}
        scroll={{ x: 'max-content' }}
        pagination={{ pageSize: 10 }}
        columns={[
          {
            title: 'version', dataIndex: 'version', className: 'mono',
            render: (v: string, row) => (
              <Space>
                <span className="mono">{v}</span>
                {row.active && <Tag color="green">active</Tag>}
              </Space>
            ),
          },
          { title: 'source', dataIndex: 'source' },
          { title: 'documents', dataIndex: 'documents', align: 'right' },
          { title: 'created', dataIndex: 'createdAt', render: when },
          { title: 'by', dataIndex: 'createdBy' },
          { title: 'note', dataIndex: 'note' },
          ...(canWrite ? [{
            title: '', key: 'rollback', width: 110,
            render: (_: unknown, row: CorpusVersion) => row.active ? null : (
              <Popconfirm
                title="Activate this version?"
                description="Customers are answered from it immediately."
                onConfirm={async () => {
                  if (!data) return
                  try {
                    await api.activateVersion(row.version, data.revision)
                    void reload()
                    void versions.reload()
                  } catch (err) {
                    setProblem(err instanceof ApiError ? err.message : String(err))
                  }
                }}
              >
                <Button size="small">Activate</Button>
              </Popconfirm>
            ),
          }] : []),
        ]}
      />

      {editing && (
        <EntryEditor
          entry={editing}
          onClose={() => setEditing(null)}
          onSaved={() => { setEditing(null); void reload() }}
        />
      )}
    </>
  )
}

function EntryEditor(props: {
  entry: KnowledgeEntry
  onClose: () => void
  onSaved: () => void
}) {
  const [entry, setEntry] = useState(props.entry)
  const [saving, setSaving] = useState(false)
  const [problem, setProblem] = useState('')
  const creating = props.entry.entryId === ''

  const save = async () => {
    setSaving(true)
    setProblem('')
    try {
      await api.saveEntry(entry)
      props.onSaved()
    } catch (err) {
      setProblem(err instanceof ApiError ? err.message : String(err))
    } finally {
      setSaving(false)
    }
  }

  return (
    <Modal
      open
      width={720}
      title={creating ? 'New entry' : `${entry.entryId} · ${entry.language}`}
      onCancel={props.onClose}
      onOk={save}
      okText="Save draft"
      confirmLoading={saving}
    >
      {problem && <Alert type="error" showIcon message={problem} style={{ marginBottom: 12 }} />}
      <Typography.Paragraph type="secondary">
        Saving changes a draft. Customers see it when somebody publishes.
      </Typography.Paragraph>
      <Form layout="vertical">
        <Space>
          <Form.Item label="Entry id">
            <Input
              disabled={!creating}
              value={entry.entryId}
              onChange={(e) => setEntry({ ...entry, entryId: e.target.value })}
            />
          </Form.Item>
          <Form.Item label="Language">
            <Input
              disabled={!creating}
              style={{ width: 90 }}
              value={entry.language}
              onChange={(e) => setEntry({ ...entry, language: e.target.value })}
            />
          </Form.Item>
          <Form.Item label="Category">
            <Input
              value={entry.category}
              onChange={(e) => setEntry({ ...entry, category: e.target.value })}
            />
          </Form.Item>
        </Space>
        <Form.Item label="Question">
          <Input.TextArea
            rows={2}
            value={entry.question}
            onChange={(e) => setEntry({ ...entry, question: e.target.value })}
          />
        </Form.Item>
        <Form.Item
          label="Answer"
          help={`${entry.answer.length} / ${MAX_ANSWER_LENGTH} characters`}
          validateStatus={entry.answer.length > MAX_ANSWER_LENGTH ? 'error' : undefined}
        >
          <Input.TextArea
            rows={8}
            value={entry.answer}
            onChange={(e) => setEntry({ ...entry, answer: e.target.value })}
          />
        </Form.Item>
      </Form>
      {/* Said on the page because it is the thing an editor most needs to know and least
          expects: what goes in here is read by a model that can call tools. */}
      <Alert
        type="warning"
        showIcon
        message="This text is read by the assistant when it answers."
        description="Write answers, not instructions to the assistant. Anything that reads like a command is still just text a customer's question can pull into the conversation."
      />
    </Modal>
  )
}
