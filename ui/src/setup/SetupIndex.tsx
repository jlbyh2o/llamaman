import { useEffect } from 'react';
import { useNavigate } from '@tanstack/react-router';
import { LoadingPanel } from '../components';
import { useWizard } from './useWizard';

/**
 * `/setup` itself: the resume point.
 *
 * It renders nothing — it reads `wizard_steps` and forwards to whichever step the server says is
 * active, which is what makes "a browser refresh or a daemon restart mid-build does not restart the
 * wizard" (DESIGN section 11.2) true without the client remembering anything.
 */
export function SetupIndex() {
  const navigate = useNavigate();
  const wizard = useWizard();

  useEffect(() => {
    if (wizard.isPending) return;
    void navigate({ to: wizard.resumePath, replace: true });
  }, [wizard.isPending, wizard.resumePath, navigate]);

  return <LoadingPanel>Finding where you left off…</LoadingPanel>;
}
