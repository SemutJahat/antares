import { memo, useEffect, useState } from 'react'
import {
  ArrowClockwise,
  Brain,
  CaretDown,
  Check,
  Copy,
  FileText,
  PencilSimple,
  Terminal,
  Warning,
} from '@phosphor-icons/react'
import { copyText } from '@/lib/clipboard'
import { useI18n, useTimeAgo } from '@/lib/i18n'
import { cn } from '@/lib/utils'
import { Button } from '@/components/ui/button'
import { Markdown } from '@/components/chat/Markdown'
import { ToolCallCard } from '@/components/chat/ToolCallCard'
import { AskUserCard } from '@/components/chat/AskUserCard'
import type { ChatMessage } from '@/lib/chatTranscript'

/**
 * Collapsible model-thinking block.
 *
 * Must NOT run the chat Markdown renderer on expand: reasoning traces are long
 * (tens of KB of decompiler/code-like text with many `*`/`[]`), and turning
 * that into hundreds of React nodes freezes the tab ("Page Unresponsive").
 * Plain pre-wrap text in a height-capped scroller is one DOM node, cheap to
 * open, and matches how thinking logs are meant to be read.
 *
 * Memoised for the same reason as ToolCallCard: on a message that grows to many
 * segments during one streaming turn, only the changed segment should re-render.
 * `text` is a primitive, so memo compares by value and finished blocks are free.
 */
const ReasoningBlock = memo(function ReasoningBlock({ text }: { text: string }) {
  const { t } = useI18n()
  const [open, setOpen] = useState(false)
  // Defer mounting the body to the next frame so the click paints first and
  // Chrome does not treat the expand as a long task on the same turn.
  const [bodyReady, setBodyReady] = useState(false)
  useEffect(() => {
    if (!open) {
      setBodyReady(false)
      return
    }
    const id = requestAnimationFrame(() => setBodyReady(true))
    return () => cancelAnimationFrame(id)
  }, [open])

  return (
    <div className="text-muted-foreground">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex items-center gap-1.5 text-[11px] font-medium transition-colors hover:text-foreground"
      >
        <Brain className="size-3.5" />
        {t('chat.reasoning')}
        {text.length > 2000 ? (
          <span className="font-normal text-muted-foreground/70">
            ({Math.round(text.length / 1000)}k)
          </span>
        ) : null}
        <CaretDown className={cn('size-3 transition-transform', open && 'rotate-180')} />
      </button>
      {open ? (
        <div className="mt-1.5 max-h-80 overflow-y-auto overflow-x-hidden border-l-2 border-border pl-3">
          {bodyReady ? (
            <pre className="m-0 whitespace-pre-wrap break-words font-mono text-[11px] leading-relaxed text-muted-foreground">
              {text}
            </pre>
          ) : (
            <p className="m-0 text-[11px] text-muted-foreground/60">…</p>
          )}
        </div>
      ) : null}
    </div>
  )
})

// A plain-text segment. Memoised so it is skipped when an unrelated segment on
// the same message changes during streaming.
const TextSegment = memo(function TextSegment({ text }: { text: string }) {
  return (
    <div className="text-[13px] leading-relaxed">
      <Markdown content={text} />
    </div>
  )
})

export function ErrorBanner({ message, className }: { message: string; className?: string }) {
  return (
    <div
      className={cn(
        'flex items-start gap-2 rounded-[var(--radius-sm)] border border-destructive/40 bg-destructive/10 p-3 text-xs text-destructive',
        className,
      )}
    >
      <Warning className="mt-0.5 size-4 shrink-0" weight="fill" />
      <span className="min-w-0 break-words">{message}</span>
    </div>
  )
}

/** Terminal-turn error: renders the JSON payload verbatim in a monospace
 *  block with Copy (of the exact JSON) and optional Retry. Not run through
 *  Markdown — a `**` inside an error message would break the format. */
export function AssistantErrorBlock({
  payload,
  onCopy,
  onRetry,
  retryDisabled,
}: {
  payload: string
  onCopy: () => Promise<boolean> | boolean | void
  onRetry?: () => void
  retryDisabled?: boolean
}) {
  const { t } = useI18n()
  const [copied, setCopied] = useState(false)
  const doCopy = async () => {
    if (await onCopy()) {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    }
  }
  return (
    <div className="rounded-[var(--radius-sm)] border border-destructive/40 bg-destructive/10 text-destructive">
      <div className="flex items-center justify-between gap-2 border-b border-destructive/30 px-3 py-2">
        <div className="flex items-center gap-2 text-xs font-medium">
          <Warning className="size-4 shrink-0" weight="fill" />
          <span>{t('chat.errorLabel')}</span>
        </div>
        <div className="flex items-center gap-1">
          <button
            type="button"
            onClick={doCopy}
            title={t('chat.copyError')}
            aria-label={t('chat.copyError')}
            className="inline-flex min-h-11 min-w-11 items-center justify-center gap-1.5 rounded-[var(--radius-sm)] px-2 text-xs font-medium transition-colors hover:bg-destructive/15 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-destructive"
          >
            {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
            <span className="hidden sm:inline">{copied ? t('common.copied') : t('common.copy')}</span>
          </button>
          {onRetry ? (
            <button
              type="button"
              onClick={onRetry}
              disabled={retryDisabled}
              title={t('chat.retry')}
              aria-label={t('chat.retry')}
              className="inline-flex min-h-11 min-w-11 items-center justify-center gap-1.5 rounded-[var(--radius-sm)] px-2 text-xs font-medium transition-colors hover:bg-destructive/15 disabled:cursor-not-allowed disabled:opacity-50 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-destructive"
            >
              <ArrowClockwise className="size-3.5" />
              <span className="hidden sm:inline">{t('chat.retry')}</span>
            </button>
          ) : null}
        </div>
      </div>
      <pre className="m-0 max-h-64 overflow-auto whitespace-pre-wrap break-words px-3 py-2 font-mono text-[11px] leading-relaxed">
        {payload}
      </pre>
    </div>
  )
}

// Memoised: a streaming turn mutates only the last message, but setMessages
// hands a new array each token. Without memo every bubble in a long transcript
// re-renders per token — the main source of lag. With stable props (message
// reference unchanged for old turns, onAnswer via useCallback), React skips
// them and only the changed bubble re-renders.
export const MessageBubble = memo(function MessageBubble({
  message,
  showReasoning = true,
  askActive,
  onAnswer,
  onEdit,
  onRetry,
  retryDisabled,
}: {
  message: ChatMessage
  /** When false, hide reasoning blocks (display.show_reasoning). */
  showReasoning?: boolean
  // Whether an ask_user question is still awaiting an answer. When false the
  // card locks (already answered, or the run ended).
  askActive?: boolean
  onAnswer?: (text: string) => void
  // Edit this (user) message: re-send from here, optionally reverting file
  // changes made since. Absent while streaming or for a local optimistic msg.
  onEdit?: (id: string, content: string) => void
  /** Retry the failed turn this error belongs to — resends the preceding user
   *  prompt and its attachments. Only wired on messages that carry an error. */
  onRetry?: (assistantMessageId: string) => void
  /** Disables the Retry button while another run is in flight so a click
   *  cannot fire a second turn on top of a live one. */
  retryDisabled?: boolean
}) {
  const { t } = useI18n()
  const timeAgo = useTimeAgo()
  const [copied, setCopied] = useState(false)

  const copy = async () => {
    if (await copyText(message.content)) {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    }
  }

  if (message.role === 'user') {
    // Full-width, quietly set apart with a left rule and a faint tint — the
    // model's replies own the column, the prompt sits above them as context.
    return (
      <div className="fade-up space-y-2">
        {message.images?.length ? (
          <div className="flex flex-wrap gap-2">
            {message.images.map((src, i) => (
              <img
                key={i}
                src={src}
                alt=""
                className="max-h-48 rounded-[var(--radius-md)] border border-border object-contain"
              />
            ))}
          </div>
        ) : null}
        {message.docs?.length ? (
          <div className="flex flex-wrap gap-2">
            {message.docs.map((d, i) => (
              <div
                key={i}
                className="flex max-w-56 items-center gap-1.5 rounded-[var(--radius-sm)] border border-border bg-muted/40 px-2 py-1 text-xs"
              >
                <FileText className="size-4 shrink-0 text-muted-foreground" />
                <span className="truncate" title={d.name}>
                  {d.name}
                </span>
              </div>
            ))}
          </div>
        ) : null}
        {message.content ? (
          <div className="group/user relative rounded-[var(--radius-md)] border-l-2 border-primary bg-muted/40 px-3.5 py-2.5">
            <p className="whitespace-pre-wrap break-words text-[13px] leading-relaxed text-foreground">
              {message.content}
            </p>
            {/* Edit affordance: re-send the conversation from this message,
                optionally reverting file changes made after it. Only when an
                onEdit handler is wired and the message has a real (persisted) id
                — a local optimistic id cannot be edited server-side yet. */}
            {onEdit && !message.id.startsWith('local_') && !message.id.startsWith('you_') ? (
              <button
                onClick={() => onEdit(message.id, message.content)}
                title={t('edit.button')}
                aria-label={t('edit.button')}
                className="absolute -top-2 right-2 hidden items-center gap-1 rounded-full border border-border bg-background px-2 py-0.5 text-[10px] text-muted-foreground shadow-sm transition-colors hover:text-foreground group-hover/user:flex"
              >
                <PencilSimple className="size-3" />
                {t('edit.button')}
              </button>
            ) : null}
          </div>
        ) : null}
      </div>
    )
  }

  // Slash-command output is not the model talking. Setting it apart keeps the
  // transcript honest about what came from where.
  if (message.role === 'system') {
    return (
      <div className="fade-up rounded-[var(--radius-md)] border border-border bg-muted/40 px-3.5 py-3">
        <div className="mb-1.5 flex items-center gap-1.5 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
          <Terminal className="size-3" />
          {t('chat.command')}
        </div>
        <div className="text-xs">
          <Markdown content={message.content} />
        </div>
      </div>
    )
  }

  return (
    <div className="group min-w-0 space-y-1.5 fade-up">
      {message.segments && message.segments.length > 0
        ? message.segments.map((seg, i) => {
            if (seg.kind === 'reasoning') {
              if (!showReasoning) return null
              return <ReasoningBlock key={`r${i}`} text={seg.text} />
            }
            if (seg.kind === 'tool') {
              // todo calls surface in the sticky TaskBar, not inline.
              if (seg.call.name === 'todo') return null
              // ask_user renders as a question with clickable answers instead
              // of a raw tool card.
              if (seg.call.name === 'ask_user') {
                return (
                  <AskUserCard
                    key={seg.call.id}
                    call={seg.call}
                    disabled={!askActive}
                    onAnswer={onAnswer ?? (() => {})}
                  />
                )
              }
              return <ToolCallCard key={seg.call.id} call={seg.call} />
            }
            return <TextSegment key={`t${i}`} text={seg.text} />
          })
        : // Fallback for any message that predates the timeline model.
          <>
            {showReasoning && message.reasoning ? (
              <ReasoningBlock text={message.reasoning} />
            ) : null}
            {message.toolCalls?.map((call) =>
              call.name === 'todo' ? null : call.name === 'ask_user' ? (
                <AskUserCard key={call.id} call={call} disabled={!askActive} onAnswer={onAnswer ?? (() => {})} />
              ) : (
                <ToolCallCard key={call.id} call={call} />
              ),
            )}
            {message.content ? (
              <div className="text-[13px] leading-relaxed">
                <Markdown content={message.content} />
              </div>
            ) : null}
          </>}

      {message.error ? (
        <AssistantErrorBlock
          payload={message.error}
          onCopy={() => copyText(message.error ?? '')}
          onRetry={onRetry ? () => onRetry(message.id) : undefined}
          retryDisabled={retryDisabled}
        />
      ) : null}

      {message.content ? (
        <div className="flex items-center gap-2 opacity-0 transition-opacity focus-within:opacity-100 group-hover:opacity-100">
          <Button variant="ghost" size="icon-sm" onClick={copy} aria-label={t('common.copy')}>
            {copied ? (
              <Check className="size-3.5 text-[var(--success)]" />
            ) : (
              <Copy className="size-3.5" />
            )}
          </Button>
          {message.tokensOut ? (
            <span className="text-[10px] text-muted-foreground">
              {t('chat.tokensOut', { n: message.tokensOut })}
            </span>
          ) : null}
          {message.createdAt ? (
            <span className="text-[10px] text-muted-foreground">{timeAgo(message.createdAt)}</span>
          ) : null}
        </div>
      ) : null}
    </div>
  )
})
