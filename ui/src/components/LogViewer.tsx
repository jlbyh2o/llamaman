import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import type { ReactNode, UIEvent } from 'react';
import { AlertTriangle, ArrowDownToLine, Copy, Pause, Play } from 'lucide-react';
import { Button } from './Button';
import { cn } from './cn';

/**
 * The log pane.
 *
 * One component serves the three places DESIGN section 4 puts logs — the llama.cpp build log
 * (screen 12: "virtualized build-log viewer, ANSI-stripped, auto-scroll, failing step highlighted,
 * jump-to-first-error"), the instance journald pane (screen 6) and the system journal (screen 16) —
 * because they are the same problem: thousands of lines arriving live.
 *
 * Virtualization is a fixed-row-height window computed from the scroll offset, which needs no
 * dependency and is exact here: every line is one row of monospace text at a known line height.
 * Wrapping is off for the same reason, and a long line scrolls horizontally instead — which is also
 * what makes a compiler error legible.
 *
 * Follow is sticky-at-the-bottom: it stays on while the reader is at the end and turns itself off
 * the moment they scroll up, so reading history is never fought by arriving output.
 */

const ROW_HEIGHT = 18;
const OVERSCAN = 12;

/**
 * ANSI escapes, written with explicit \u001B rather than pasted control bytes: CSI (ESC [ … final),
 * OSC (ESC ] … BEL or ST) and the two-character sequences a build leaves behind. Carriage returns go
 * too — a \r-redrawn progress bar is one line on a terminal and N lines in a log pane.
 */
const ANSI_OSC = /\u001B\][^\u0007\u001B]*(?:\u0007|\u001B\\)/g;
const ANSI_CSI = /\u001B\[[0-?]*[ -/]*[@-~]/g;
const ANSI_SHORT = /\u001B[@-Z\\-_]/g;

export function stripAnsi(line: string): string {
  return line
    .replace(ANSI_OSC, '')
    .replace(ANSI_CSI, '')
    .replace(ANSI_SHORT, '')
    .replace(/\r/g, '');
}

export type LineKind = 'error' | 'warn' | 'step' | 'plain';

/**
 * Classify one line. Deliberately conservative: a false "error" in a build log sends someone
 * hunting for a problem that is not there, so only the shapes cmake, ninja, gcc and llama.cpp
 * actually print are matched.
 */
export function classifyLine(line: string): LineKind {
  if (/\b(error|fatal error|FAILED|undefined reference|Segmentation fault)\b/i.test(line)) {
    return 'error';
  }
  if (/\bwarning\b/i.test(line)) return 'warn';
  if (/^(\[\d+\/\d+\]|-- |==> |Step \d)/.test(line)) return 'step';
  return 'plain';
}

const KIND_CLASS: Record<LineKind, string> = {
  error: 'text-[var(--lm-danger)]',
  warn: 'text-[var(--lm-warn)]',
  step: 'text-[var(--lm-accent)]',
  plain: 'text-[var(--lm-text-muted)]',
};

export interface LogViewerProps {
  /** Raw lines, oldest first. ANSI is stripped here, so callers may pass them untouched. */
  lines: readonly string[];
  /** Rows tall. 24 rows ≈ a terminal; the build log uses 32. */
  rows?: number;
  /** Follow the tail. Controlled when `onFollowChange` is given. */
  follow?: boolean;
  onFollowChange?: (follow: boolean) => void;
  /** Shown above the pane: a state badge, a step name, a byte count. */
  toolbar?: ReactNode;
  /** Shown when there is nothing yet — "waiting for output". */
  placeholder?: ReactNode;
  /** Line numbers off for journald, on for a build log. */
  lineNumbers?: boolean;
  className?: string;
  'aria-label'?: string;
}

export function LogViewer({
  lines,
  rows = 24,
  follow: followProp,
  onFollowChange,
  toolbar,
  placeholder = 'Waiting for output…',
  lineNumbers = true,
  className,
  ...aria
}: LogViewerProps) {
  const viewportRef = useRef<HTMLDivElement | null>(null);
  const [scrollTop, setScrollTop] = useState(0);
  const [internalFollow, setInternalFollow] = useState(true);
  const follow = followProp ?? internalFollow;

  const setFollow = useCallback(
    (next: boolean) => {
      setInternalFollow(next);
      onFollowChange?.(next);
    },
    [onFollowChange],
  );

  const clean = useMemo(() => lines.map(stripAnsi), [lines]);
  const kinds = useMemo(() => clean.map(classifyLine), [clean]);
  const firstError = useMemo(() => kinds.indexOf('error'), [kinds]);

  const height = rows * ROW_HEIGHT;
  const total = clean.length * ROW_HEIGHT;
  const start = Math.max(0, Math.floor(scrollTop / ROW_HEIGHT) - OVERSCAN);
  const end = Math.min(clean.length, Math.ceil((scrollTop + height) / ROW_HEIGHT) + OVERSCAN);

  // Pin to the bottom whenever new lines land while following.
  useLayoutEffect(() => {
    if (!follow) return;
    const viewport = viewportRef.current;
    if (!viewport) return;
    viewport.scrollTop = viewport.scrollHeight;
    setScrollTop(viewport.scrollTop);
  }, [follow, clean.length]);

  const onScroll = (event: UIEvent<HTMLDivElement>) => {
    const el = event.currentTarget;
    setScrollTop(el.scrollTop);
    // Anything but "at the bottom" means the reader has taken over.
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < ROW_HEIGHT;
    if (atBottom !== follow) setFollow(atBottom);
  };

  const scrollToLine = useCallback((index: number) => {
    const viewport = viewportRef.current;
    if (!viewport) return;
    viewport.scrollTop = Math.max(0, index * ROW_HEIGHT - viewport.clientHeight / 3);
    setScrollTop(viewport.scrollTop);
  }, []);

  const jumpToFirstError = useCallback(() => {
    if (firstError < 0) return;
    setFollow(false);
    scrollToLine(firstError);
  }, [firstError, scrollToLine, setFollow]);

  const copyAll = useCallback(() => {
    void navigator.clipboard?.writeText(clean.join('\n'));
  }, [clean]);

  // Keep the window in sync when the pane is resized out from under us.
  useEffect(() => {
    const viewport = viewportRef.current;
    if (!viewport) return;
    setScrollTop(viewport.scrollTop);
  }, [rows]);

  const gutter = lineNumbers ? String(clean.length).length : 0;

  return (
    <div className={cn('flex flex-col', className)}>
      <div className="flex items-center justify-between gap-2 border-b border-[var(--lm-border)] px-3 py-1.5">
        <div className="flex min-w-0 items-center gap-2 text-xs text-[var(--lm-text-muted)]">
          {toolbar}
        </div>
        <div className="flex shrink-0 items-center gap-1">
          {firstError >= 0 ? (
            <Button
              size="sm"
              variant="ghost"
              icon={<AlertTriangle />}
              onClick={jumpToFirstError}
              className="text-[var(--lm-danger)]"
            >
              First error
            </Button>
          ) : null}
          <Button size="sm" variant="ghost" icon={<Copy />} onClick={copyAll}>
            Copy
          </Button>
          <Button
            size="sm"
            variant="ghost"
            icon={follow ? <Pause /> : <Play />}
            aria-pressed={follow}
            onClick={() => {
              const next = !follow;
              setFollow(next);
              if (next) scrollToLine(clean.length);
            }}
          >
            {follow ? 'Following' : 'Follow'}
          </Button>
        </div>
      </div>

      <div
        ref={viewportRef}
        onScroll={onScroll}
        role="log"
        aria-live={follow ? 'polite' : 'off'}
        aria-label={aria['aria-label'] ?? 'Log output'}
        tabIndex={0}
        style={{ height }}
        className="overflow-auto bg-[var(--lm-surface-sunken)] font-[family-name:var(--lm-font-mono)] text-[12px] leading-[18px]"
      >
        {clean.length === 0 ? (
          <div className="flex h-full items-center justify-center text-xs text-[var(--lm-text-faint)]">
            {placeholder}
          </div>
        ) : (
          <div style={{ height: total, position: 'relative' }}>
            <div style={{ transform: `translateY(${start * ROW_HEIGHT}px)` }}>
              {clean.slice(start, end).map((line, offset) => {
                const index = start + offset;
                const kind = kinds[index] ?? 'plain';
                return (
                  <div
                    key={index}
                    style={{ height: ROW_HEIGHT }}
                    className={cn(
                      'flex items-center gap-3 px-3 whitespace-pre',
                      kind === 'error' && 'bg-[var(--lm-danger-soft)]',
                      index === firstError && 'ring-1 ring-[var(--lm-danger)]/40 ring-inset',
                    )}
                  >
                    {lineNumbers ? (
                      <span
                        aria-hidden
                        style={{ width: `${gutter}ch` }}
                        className="shrink-0 text-right text-[var(--lm-text-faint)] opacity-60 select-none"
                      >
                        {index + 1}
                      </span>
                    ) : null}
                    <span className={KIND_CLASS[kind]}>{line || ' '}</span>
                  </div>
                );
              })}
            </div>
          </div>
        )}
      </div>

      {!follow && clean.length > 0 ? (
        <button
          type="button"
          onClick={() => {
            setFollow(true);
            scrollToLine(clean.length);
          }}
          className="flex items-center justify-center gap-1.5 border-t border-[var(--lm-border)] py-1 text-xs text-[var(--lm-accent)] hover:bg-[var(--lm-neutral-soft)]"
        >
          <ArrowDownToLine aria-hidden className="size-3.5" />
          Jump to latest
        </button>
      ) : null}
    </div>
  );
}
