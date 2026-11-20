import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

const config: Config = {
  title: 'bridgectl Documentation',
  tagline: 'Run, attach to, and replay local or remote AI agent sessions.',

  url: 'https://bridgectl.orchael.com',
  baseUrl: '/',
  organizationName: 'orchael',
  projectName: 'bridgectl',
  trailingSlash: false,

  onBrokenLinks: 'throw',

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  markdown: {
    mermaid: true,
    hooks: {
      onBrokenMarkdownLinks: 'warn',
    },
  },

  themes: ['@docusaurus/theme-mermaid'],

  presets: [
    [
      'classic',
      {
        docs: {
          routeBasePath: '/',
          sidebarPath: './sidebars.ts',
          editUrl: 'https://github.com/orchael/bridgectl/tree/main/docs/',
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      } satisfies Preset.Options,
    ],
  ],

  themeConfig: {
    navbar: {
      title: 'AI Agent Bridge',
      items: [
        {
          type: 'docSidebar',
          sidebarId: 'tutorialSidebar',
          position: 'left',
          label: 'Docs',
        },
        {
          href: 'https://github.com/orchael/bridgectl',
          label: 'GitHub',
          position: 'right',
        },
      ],
    },
    footer: {
      style: 'dark',
      links: [
        {
          title: 'Use',
          items: [
            {label: 'Local quick start', to: '/getting-started/local-server'},
            {label: 'Web UI', to: '/guides/web-ui'},
            {label: 'Step CA over Tailscale', to: '/guides/step-ca-tailscale'},
          ],
        },
        {
          title: 'Reference',
          items: [
            {label: 'CLI', to: '/reference/cli'},
            {label: 'Configuration', to: '/reference/configuration'},
            {label: 'gRPC API', to: '/reference/grpc-api'},
          ],
        },
      ],
      copyright: `Copyright © ${new Date().getFullYear()} AI Agent Bridge contributors.`,
    },
    prism: {
      additionalLanguages: ['bash', 'go', 'protobuf', 'yaml'],
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
