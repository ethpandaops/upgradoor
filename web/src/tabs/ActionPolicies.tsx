import { list, type Policy } from '../api/types';
import { useLive } from '../components/Live';
import { actionPolicyNames } from '../lib/hostActions';
import { parsePolicies, policiesText, type FieldSpec } from '../lib/settingsForm';
import { PolicySwitch, TreeMarks, TreeRow, type FieldState } from './settingsParts';

/**
 * ActionPolicies edits the policy of each host action (actionPolicies): the
 * actions the hosts report and those that have a policy, each auto or manual.
 * An action without a policy is manual.
 */
export function ActionPolicies({ spec, state }: { spec: FieldSpec; state: FieldState }) {
  const hosts = list(useLive('hosts').data?.hosts);
  const policies = parsePolicies(state.value) ?? {};
  const names = actionPolicyNames(hosts, { ...(parsePolicies(state.stored) ?? {}), ...policies });

  const set = (name: string, policy: Policy) => {
    state.set(policiesText({ ...policies, [name]: policy }));
  };

  return (
    <div role="group" aria-label={spec.label} className="flex flex-col gap-2 py-4">
      {names.length === 0 ? (
        <p className="text-xs/5 text-muted">No host reports an action.</p>
      ) : (
        <ul className="flex flex-col">
          {names.map(name => (
            <li key={name}>
              <TreeRow depth={0} name={<span className="font-mono text-xs/5 font-medium">{name}</span>}>
                <PolicySwitch
                  label={`Policy of the action ${name}`}
                  value={policies[name] ?? 'manual'}
                  disabled={state.disabled}
                  reason={state.reason}
                  onChange={p => {
                    if (p !== 'inherit') set(name, p);
                  }}
                />
              </TreeRow>
            </li>
          ))}
        </ul>
      )}
      <TreeMarks fields={[{ spec, state }]} />
    </div>
  );
}
