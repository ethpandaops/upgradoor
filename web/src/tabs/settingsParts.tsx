import type { ReactNode } from 'react';

import type { Policy } from '../api/types';
import { Badge, InfoTip, cx } from '../components/primitives';
import {
  POLICIES,
  POLICY_LABEL,
  POLICY_MEANING,
  sameValue,
  shortValue,
  valueText,
  type FieldSpec,
} from '../lib/settingsForm';

/** What every control of the settings form needs to know about its setting. */
export interface FieldState {
  value: string;
  /** stored and fallback are the saved value and the default, as their inputs would show them. */
  stored: string;
  fallback: string;
  /** error is what is wrong with the input, or what the controller said about it. */
  error: string | undefined;
  dirty: boolean;
  disabled: boolean;
  /** reason says why the field cannot be changed, when it cannot. */
  reason?: string;
  set: (value: string) => void;
}

/** ROW_GRID is the form grid: the label column, then the control column of one width. */
export const ROW_GRID = 'grid grid-cols-1 gap-x-8 gap-y-2 sm:grid-cols-[minmax(0,1fr)_17rem]';

/** InfoText is what stands behind a setting's info icon: what it does, its valid range, its default. */
export function InfoText({ spec, fallback }: { spec: FieldSpec; fallback: string }) {
  return (
    <>
      <span className="block">{spec.hint}</span>
      {spec.range && <span className="mt-1 block text-muted">Valid: {spec.range}.</span>}
      <span className="mt-1 block text-muted">
        Default: {valueText(spec, fallback)}. Setting: <span className="font-mono">{spec.key}</span>
      </span>
    </>
  );
}

/** FieldLabel is the label column of a row: the name with its info icon, the help line under it. */
export function FieldLabel({
  spec,
  state,
  htmlFor,
  helpId,
}: {
  spec: FieldSpec;
  state: FieldState;
  /** htmlFor is the input the label names; without it the name is plain text. */
  htmlFor?: string;
  helpId: string;
}) {
  return (
    <div className="flex min-w-0 flex-col gap-0.5">
      <div className="flex items-center gap-1.5">
        {htmlFor ? (
          <label htmlFor={htmlFor} className="text-sm/6 font-medium">
            {spec.label}
          </label>
        ) : (
          <span className="text-sm/6 font-medium">{spec.label}</span>
        )}
        <InfoTip label={spec.label}>
          <InfoText spec={spec} fallback={state.fallback} />
        </InfoTip>
      </div>
      <p id={helpId} className="text-xs/5 text-muted">
        {spec.help}
      </p>
    </div>
  );
}

/**
 * Mark is a line under a control, only while there is something to say: that
 * the value is changed and not saved, and that it differs from the default,
 * with the default. The default is a button that puts it back.
 */
export function Mark({ spec, state }: { spec: FieldSpec; state: FieldState }) {
  const isDefault = sameValue(spec, state.value, state.fallback);
  if (!state.dirty && isDefault) return null;

  return (
    <span className="flex flex-wrap items-center gap-1.5 text-xs/5">
      {state.dirty && <Badge tone="info">changed</Badge>}
      {!isDefault && (
        <button
          type="button"
          disabled={state.disabled}
          aria-label={`Reset ${spec.label} to its default`}
          title="This is not the default. Click to put the default back."
          onClick={() => {
            state.set(state.fallback);
          }}
          className="rounded-xs px-1 text-warning hover:bg-surface-hover disabled:cursor-not-allowed"
        >
          default {shortValue(spec, state.fallback)}
        </button>
      )}
    </span>
  );
}

/** Problem is what is wrong with an input, under it. It is there only while something is wrong. */
export function Problem({ state }: { state: FieldState }) {
  if (!state.error) return null;

  return (
    <span role="alert" className="text-xs/5 text-danger">
      {state.error}
    </span>
  );
}

/** FieldRow is one setting in the form grid: label and help on the left, the control and its notes on the right. */
export function FieldRow({ label, children }: { label: ReactNode; children: ReactNode }) {
  return (
    <div data-setting className={cx(ROW_GRID, 'py-4 sm:items-start')}>
      {label}
      <div className="flex min-w-0 flex-col items-start gap-1">{children}</div>
    </div>
  );
}

/** SwitchChoice is a policy, or inherit: the entry follows its parent. */
export type SwitchChoice = Policy | 'inherit';

const SWITCH_LABEL: Record<SwitchChoice, string> = { inherit: 'Inherit', auto: 'Auto', manual: 'Manual' };

/**
 * PolicySwitch is a policy as a segmented switch. With inherited it offers
 * inherit first; while it inherits, the policy it follows is outlined.
 */
export function PolicySwitch({
  label,
  value,
  inherited,
  disabled,
  reason,
  onChange,
}: {
  label: string;
  value: SwitchChoice;
  /** inherited is the parent's policy; without it there is no inherit. */
  inherited?: Policy;
  disabled: boolean;
  /** reason says why a disabled switch cannot be changed. */
  reason?: string;
  onChange: (choice: SwitchChoice) => void;
}) {
  const options: SwitchChoice[] = inherited ? ['inherit', ...POLICIES] : [...POLICIES];

  return (
    <div
      role="group"
      aria-label={label}
      className="inline-flex shrink-0 divide-x divide-border-strong rounded-sm border border-border-strong"
    >
      {options.map(o => {
        const pressed = o === value;
        const ghost = value === 'inherit' && o === inherited;
        return (
          <button
            key={o}
            type="button"
            aria-pressed={pressed}
            disabled={disabled}
            title={
              disabled && reason
                ? reason
                : o === 'inherit'
                  ? `Inherit: ${inherited ?? ''}`
                  : `${POLICY_LABEL[o]}: ${POLICY_MEANING[o]}`
            }
            onClick={() => {
              if (!pressed) onChange(o);
            }}
            className={cx(
              'h-7 min-w-14 px-2.5 text-xs/5 font-medium first:rounded-l-xs last:rounded-r-xs disabled:cursor-not-allowed max-sm:h-9',
              pressed
                ? 'bg-surface-selected text-foreground'
                : ghost
                  ? 'text-foreground outline-1 -outline-offset-4 outline-accent outline-dashed'
                  : 'text-muted enabled:hover:bg-surface-hover',
              disabled && !pressed && !ghost && 'opacity-50'
            )}
          >
            {SWITCH_LABEL[o]}
          </button>
        );
      })}
    </div>
  );
}

/**
 * TreeRow is one row of a policy tree: its name indented by depth with guide
 * lines, its switch in the control column, so the switches of all levels line
 * up. On a narrow window the switch stands at the end of the row.
 */
export function TreeRow({
  depth,
  name,
  extra,
  label,
  children,
}: {
  depth: number;
  name: ReactNode;
  extra?: ReactNode;
  /** label names the row for assistive tech. */
  label?: string;
  children: ReactNode;
}) {
  return (
    <div
      aria-label={label}
      className="flex flex-wrap items-stretch justify-between gap-x-2 sm:grid sm:grid-cols-[minmax(0,1fr)_17rem] sm:gap-x-8"
    >
      <div className="flex min-w-0 items-stretch">
        {Array.from({ length: depth }, (_, i) => (
          <span key={i} aria-hidden="true" className="ml-2 w-3 shrink-0 border-l border-border" />
        ))}
        <div className="flex min-w-0 flex-wrap items-center gap-x-2 py-1.5">
          {name}
          {extra}
        </div>
      </div>
      <div className="ml-auto flex items-center py-1.5 sm:ml-0">{children}</div>
    </div>
  );
}

/**
 * TreeMarks are the notes of the settings a tree edits, under it: changed and
 * not saved, not the default (with a reset), what is wrong.
 */
export function TreeMarks({ fields }: { fields: readonly { spec: FieldSpec; state: FieldState }[] }) {
  const shown = fields.flatMap(({ spec, state }) => {
    const isDefault = sameValue(spec, state.value, state.fallback);
    return state.dirty || state.error || !isDefault ? [{ spec, state, isDefault }] : [];
  });
  if (shown.length === 0) return null;

  return (
    <div className="flex flex-col gap-1 border-t border-border pt-2">
      {shown.map(({ spec, state, isDefault }) => (
        <div key={spec.key} className="flex flex-wrap items-center gap-x-2 text-xs/5">
          <span className="text-muted">{spec.label}</span>
          {state.dirty && <Badge tone="info">changed</Badge>}
          {!isDefault && (
            <button
              type="button"
              disabled={state.disabled}
              aria-label={`Reset ${spec.label} to its default`}
              title={`Default: ${valueText(spec, state.fallback)}`}
              onClick={() => {
                state.set(state.fallback);
              }}
              className="rounded-xs px-1 text-warning hover:bg-surface-hover disabled:cursor-not-allowed"
            >
              reset to default
            </button>
          )}
          <Problem state={state} />
        </div>
      ))}
    </div>
  );
}
