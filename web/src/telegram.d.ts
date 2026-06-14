interface TelegramWebApp {
  platform: string;
  isFullscreen: boolean;
  contentSafeAreaInset: TelegramSafeAreaInset;
  ready(): void;
  expand(): void;
  isVersionAtLeast(version: string): boolean;
  requestFullscreen(): void;
  onEvent(eventType: TelegramWebAppEvent, eventHandler: () => void): void;
  offEvent(eventType: TelegramWebAppEvent, eventHandler: () => void): void;
  setHeaderColor(color: string): void;
  setBackgroundColor(color: string): void;
  setBottomBarColor(color: string): void;
  HapticFeedback?: {
    impactOccurred(style: "light" | "medium" | "heavy" | "rigid" | "soft"): void;
    notificationOccurred(type: "error" | "success" | "warning"): void;
  };
}

interface TelegramSafeAreaInset {
  top: number;
  bottom: number;
  left: number;
  right: number;
}

type TelegramWebAppEvent = "fullscreenChanged" | "contentSafeAreaChanged";

interface Window {
  Telegram?: {
    WebApp: TelegramWebApp;
  };
}
