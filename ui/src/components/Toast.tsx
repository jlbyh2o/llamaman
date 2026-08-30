import * as ToastPrimitive from '@radix-ui/react-toast';
import { AlertTriangle, CheckCircle2, Info, XCircle, X } from 'lucide-react';
import type { ReactNode } from 'react';
import { create } from 'zustand';
import { ApiError, TransportError } from '../api/errors';
import { cn } from './cn';
import type { Tone } from './Badge';

/**
 * Transient notices.
 *
 * A toast reports the *outcome* of something the user just did. It is never where a durable fact
 * lives: those are `notifications` rows with remediation cards (DESIGN section 2.11, section 17),
 * which survive a reload and a different browser, and which this component deliberately does not
 * replace.
 *
 * `toast.error(err)` understands the error envelope, so a handler is one line and the closed `code`
 * reaches the screen instead of a stringified exception.
 */

export type ToastTone = Extract<Tone, 'neutral' | 'ok' | 'warn' | 'danger' | 'info'>;

export interface ToastItem {
  id: string;
  tone: ToastTone;
  title: ReactNode;
  description?: ReactNode;
  /** A one-click follow-up: "View job", "Retry", "Copy command". */
  action?: { label: string; onClick: () => void };
  /** Milliseconds; `null` keeps it until dismissed, which errors do by default. */
  duration: number | null;
}

interface ToastStore {
  items: ToastItem[];
  push: (item: Omit<ToastItem, 'id'>) => string;
  dismiss: (id: string) => void;
  clear: () => void;
}

let counter = 0;

export const useToastStore = create<ToastStore>((set) => ({
  items: [],
  push: (item) => {
    counter += 1;
    const id = `toast-${counter}`;
    set((state) => ({ items: [...state.items, { ...item, id }] }));
    return id;
  },
  dismiss: (id) => set((state) => ({ items: state.items.filter((t) => t.id !== id) })),
  clear: () => set({ items: [] }),
}));

type ToastOptions = Partial<Pick<ToastItem, 'description' | 'action' | 'duration'>>;

function push(tone: ToastTone, title: ReactNode, options: ToastOptions = {}): string {
  const { description, action, duration } = options;
  return useToastStore.getState().push({
    tone,
    title,
    duration: duration === undefined ? (tone === 'danger' ? null : 5000) : duration,
    ...(description === undefined ? {} : { description }),
    ...(action === undefined ? {} : { action }),
  });
}

/** Turn any thrown value into a title and a description worth showing. */
export function describeError(error: unknown): { title: string; description: string } {
  if (error instanceof ApiError) {
    return { title: error.message || error.code, description: error.code };
  }
  if (error instanceof TransportError) {
    return { title: error.message, description: error.request };
  }
  if (error instanceof Error) return { title: error.message, description: error.name };
  return { title: 'Something went wrong', description: String(error) };
}

export const toast = {
  success: (title: ReactNode, options?: ToastOptions) => push('ok', title, options),
  info: (title: ReactNode, options?: ToastOptions) => push('info', title, options),
  warn: (title: ReactNode, options?: ToastOptions) => push('warn', title, options),
  message: (title: ReactNode, options?: ToastOptions) => push('neutral', title, options),
  /** Accepts a thrown value or a plain message. */
  error: (error: unknown, options?: ToastOptions) => {
    if (typeof error === 'string') return push('danger', error, options);
    const { title, description } = describeError(error);
    return push('danger', title, { description, ...options });
  },
  dismiss: (id: string) => useToastStore.getState().dismiss(id),
};

const ICONS: Record<ToastTone, typeof Info> = {
  neutral: Info,
  ok: CheckCircle2,
  warn: AlertTriangle,
  danger: XCircle,
  info: Info,
};

const ACCENT: Record<ToastTone, string> = {
  neutral: 'text-[var(--lm-text-muted)]',
  ok: 'text-[var(--lm-ok)]',
  warn: 'text-[var(--lm-warn)]',
  danger: 'text-[var(--lm-danger)]',
  info: 'text-[var(--lm-info)]',
};

/** Mount once, in the shell. Renders whatever the store holds. */
export function Toaster() {
  const items = useToastStore((state) => state.items);
  const dismiss = useToastStore((state) => state.dismiss);

  return (
    <ToastPrimitive.Provider swipeDirection="right" duration={5000}>
      {items.map((item) => {
        const Icon = ICONS[item.tone];
        return (
          <ToastPrimitive.Root
            key={item.id}
            duration={item.duration ?? Number.POSITIVE_INFINITY}
            onOpenChange={(open) => {
              if (!open) dismiss(item.id);
            }}
            className={cn(
              'flex items-start gap-3 rounded-[var(--lm-radius-lg)] border border-[var(--lm-border)]',
              'bg-[var(--lm-surface-raised)] p-3 shadow-[var(--lm-shadow)]',
              'data-[swipe=move]:translate-x-[var(--radix-toast-swipe-move-x)]',
            )}
          >
            <Icon aria-hidden className={cn('mt-0.5 size-4 shrink-0', ACCENT[item.tone])} />
            <div className="min-w-0 flex-1">
              <ToastPrimitive.Title className="text-sm font-medium text-[var(--lm-text)]">
                {item.title}
              </ToastPrimitive.Title>
              {item.description ? (
                <ToastPrimitive.Description className="mt-0.5 text-xs break-words text-[var(--lm-text-muted)]">
                  {item.description}
                </ToastPrimitive.Description>
              ) : null}
              {item.action ? (
                <ToastPrimitive.Action
                  asChild
                  altText={item.action.label}
                  onClick={item.action.onClick}
                >
                  <button
                    type="button"
                    className="mt-2 text-xs font-medium text-[var(--lm-accent)] underline-offset-4 hover:underline"
                  >
                    {item.action.label}
                  </button>
                </ToastPrimitive.Action>
              ) : null}
            </div>
            <ToastPrimitive.Close
              aria-label="Dismiss"
              className="rounded-[var(--lm-radius-sm)] p-0.5 text-[var(--lm-text-faint)] hover:text-[var(--lm-text)]"
            >
              <X aria-hidden className="size-3.5" />
            </ToastPrimitive.Close>
          </ToastPrimitive.Root>
        );
      })}
      <ToastPrimitive.Viewport
        style={{ zIndex: 'var(--lm-z-toast)' }}
        className="fixed right-4 bottom-4 flex w-[22rem] max-w-[calc(100vw-2rem)] flex-col gap-2 outline-none"
      />
    </ToastPrimitive.Provider>
  );
}
