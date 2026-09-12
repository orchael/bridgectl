import type {SidebarsConfig} from '@docusaurus/plugin-content-docs';

const sidebars: SidebarsConfig = {
  tutorialSidebar: [
    'intro',
    {
      type: 'category',
      label: 'Getting Started',
      items: ['getting-started/installation', 'getting-started/local-machine', 'getting-started/local-server'],
    },
    {
      type: 'category',
      label: 'Guides',
      items: ['guides/session-workflow', 'guides/web-ui', 'guides/step-ca-tailscale', 'guides/crew-integration'],
    },
    {
      type: 'category',
      label: 'Security',
      items: [
        'security/overview',
        'security/step-ca',
        'security/existing-ca',
        'security/certificate-rotation',
        'security/migration-v1.1',
      ],
    },
    {
      type: 'category',
      label: 'Reference',
      items: [
        'reference/cli',
        'reference/configuration',
        'reference/docker-compose',
        'reference/go-sdk',
        'reference/grpc-api',
      ],
    },
    'troubleshooting',
  ],
};

export default sidebars;
