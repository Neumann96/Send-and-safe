interface TelegramWebApp {
  ready(): void;
  expand(): void;
  setHeaderColor(color: string): void;
  setBackgroundColor(color: string): void;
}

interface Window {
  Telegram?: {
    WebApp: TelegramWebApp;
  };
}
