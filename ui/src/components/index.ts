/**
 * The kit.
 *
 * One import for every screen. Nothing here reaches for the API, the router or the query cache — a
 * kit component takes props and renders, which is what lets the component tests mount any of them
 * in either theme without a provider tree.
 */

export { Badge } from './Badge';
export type { BadgeProps, Tone } from './Badge';

export { Button } from './Button';
export type { ButtonProps, ButtonSize, ButtonVariant } from './Button';

export { cn } from './cn';

export { ConfirmDialog } from './ConfirmDialog';
export type { ConfirmDialogProps } from './ConfirmDialog';

export { compareValues, DataTable, sortRows } from './DataTable';
export type { Column, DataTableProps, SortState } from './DataTable';

export { Dialog, DialogClose, DialogContent, DialogTrigger } from './Dialog';
export type { DialogContentProps } from './Dialog';

export {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from './DropdownMenu';

export { EmptyState } from './EmptyState';
export type { EmptyStateProps } from './EmptyState';

export { FieldGroup, FormField } from './FormField';
export type { FieldRenderProps, FormFieldProps } from './FormField';

export { Input, Textarea } from './Input';
export type { InputProps, TextareaProps } from './Input';

export { classifyLine, LogViewer, stripAnsi } from './LogViewer';
export type { LineKind, LogViewerProps } from './LogViewer';

export { Field, Mono, Panel, PanelHeader } from './Panel';
export type { PanelProps, PanelHeaderProps } from './Panel';

export { QueryError } from './QueryError';
export type { QueryErrorProps } from './QueryError';

export { Meter, Progress } from './Progress';
export type { ProgressProps } from './Progress';

export { Select } from './Select';
export type { SelectOption, SelectProps } from './Select';

export { LoadingPanel, Skeleton, Spinner } from './Spinner';

export { FlagBadge, STATE_MAPS, StatusBadge, stateStyle } from './StatusBadge';
export type { StateKind, StatusBadgeProps } from './StatusBadge';

export { Switch } from './Switch';
export type { SwitchProps } from './Switch';

export { Tabs, TabsContent, TabsList, TabsTrigger } from './Tabs';

export { describeError, toast, Toaster, useToastStore } from './Toast';
export type { ToastItem, ToastTone } from './Toast';

export { Tooltip, TooltipProvider } from './Tooltip';
export type { TooltipProps } from './Tooltip';
