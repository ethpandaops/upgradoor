import { list, type Policy } from '../api/types';
import { useLive } from '../components/Live';
import { Badge, InfoTip, Segmented } from '../components/primitives';
import { actionPolicyNames } from '../lib/hostActions';
import {
  POLICIES,
  POLICY_MEANING,
  fieldSpec,
  parseActionPolicies,
  actionPoliciesText,
  valueText,
} from '../lib/settingsForm';

/** The part of a setting's form state the editor reads. */
interface PoliciesState {
  value: string;
  stored: string;
  error: string | undefined;
  dirty: boolean;
  disabled: boolean;
  set: (value: string) => void;
}

/**
 * ActionPolicies edits the policy of each host action (actionPolicies): the
 * actions the hosts report and those that have a policy, each auto or manual.
 * An action without a policy is manual.
 */
export function ActionPolicies({ state }: { state: PoliciesState }) {
  const spec = fieldSpec('actionPolicies');
  const hosts = list(useLive('hosts').data?.hosts);
  const policies = parseActionPolicies(state.value) ?? {};
  const names = actionPolicyNames(hosts, { ...(parseActionPolicies(state.stored) ?? {}), ...policies });

  const set = (name: string, policy: Policy) => {
    state.set(actionPoliciesText({ ...policies, [name]: policy }));
  };

  return (
    <div
      role="group"
      aria-label={spec.label}
      className="flex flex-col gap-2 rounded-sm border border-border bg-background p-3 sm:col-span-2 xl:col-span-5"
    >
      <div className="flex min-h-6 items-center gap-2">
        <span className="text-sm/6 font-medium whitespace-nowrap">Host actions</span>
        <InfoTip label={spec.label}>
          <span className="block">{spec.hint}</span>
          <span className="mt-1 block text-muted">
            Setting: <span className="font-mono">{spec.key}</span>
          </span>
        </InfoTip>
        {state.dirty && <Badge tone="info">changed</Badge>}
        <span className="ml-auto text-xs/5 text-muted">{valueText(spec, state.value)}</span>
      </div>
      {names.length === 0 ? (
        <p className="text-xs/5 text-muted">No host reports an action.</p>
      ) : (
        <ul className="flex flex-wrap gap-x-6 gap-y-2">
          {names.map(name => (
            <li key={name} className="flex items-center gap-2 text-xs/5">
              <span className="font-mono">{name}</span>
              <Segmented
                label={`Policy of the action ${name}`}
                value={policies[name] ?? 'manual'}
                disabled={state.disabled}
                onChange={p => {
                  set(name, p);
                }}
                options={POLICIES.map(p => ({ value: p, label: p, title: POLICY_MEANING[p] }))}
              />
            </li>
          ))}
        </ul>
      )}
      {state.error && (
        <span role="alert" className="text-xs/5 text-danger">
          {state.error}
        </span>
      )}
    </div>
  );
}
