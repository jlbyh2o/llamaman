import { Radio, WifiOff } from 'lucide-react';
import { Tooltip } from '../components';
import { cn } from '../components/cn';
import { useEventStatus } from '../events/EventStreamProvider';

/**
 * The stream indicator.
 *
 * DESIGN section 4: "if the stream drops twice it falls back to interval refetch and shows a 'live
 * updates unavailable' chip". This is that chip — and, because a broken stream is a thing a person
 * can act on, it is also the button that rebuilds it.
 *
 * It is deliberately quiet while healthy: a dot and a word, not a banner.
 */
export function LiveChip({ className }: { className?: string }) {
  const { status, reconnect } = useEventStatus();

  if (status === 'degraded') {
    return (
      <Tooltip
        wide
        content="The event stream is down, so this page is refreshing on a timer instead. Data may be a few seconds behind."
      >
        <button
          type="button"
          onClick={reconnect}
          className={cn(
            'inline-flex items-center gap-1.5 rounded-[var(--lm-radius-full)] px-2 py-0.5 text-xs',
            'bg-[var(--lm-warn-soft)] text-[var(--lm-warn)] ring-1 ring-[var(--lm-warn)]/35 ring-inset',
            className,
          )}
        >
          <WifiOff aria-hidden className="size-3" />
          Live updates unavailable
        </button>
      </Tooltip>
    );
  }

  const connecting = status !== 'live';
  return (
    <Tooltip
      content={
        connecting
          ? 'Connecting to the event stream…'
          : 'Live: this page is patched by server events, not polled.'
      }
    >
      <span
        className={cn(
          'inline-flex items-center gap-1.5 rounded-[var(--lm-radius-full)] px-2 py-0.5 text-xs',
          'text-[var(--lm-text-faint)]',
          className,
        )}
      >
        <Radio
          aria-hidden
          className={cn('size-3', connecting ? 'animate-pulse' : 'text-[var(--lm-ok)]')}
        />
        {connecting ? 'Connecting' : 'Live'}
      </span>
    </Tooltip>
  );
}
