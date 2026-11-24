"use client";

import * as React from "react";
import { toast } from "sonner";

import { api } from "@/lib/api";
import type { BrowserNotifyEvent } from "@/lib/types";

const CURSOR_STORAGE_KEY = "vestrail.browser_notify.last_seq";

function playChime() {
  try {
    const AudioCtx = window.AudioContext || (window as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext;
    if (!AudioCtx) return;
    const ctx = new AudioCtx();
    const osc = ctx.createOscillator();
    const gain = ctx.createGain();
    osc.type = "sine";
    osc.frequency.setValueAtTime(880, ctx.currentTime);
    gain.gain.setValueAtTime(0.06, ctx.currentTime);
    gain.gain.exponentialRampToValueAtTime(0.001, ctx.currentTime + 0.22);
    osc.connect(gain);
    gain.connect(ctx.destination);
    osc.start();
    osc.stop(ctx.currentTime + 0.24);
    setTimeout(() => { void ctx.close(); }, 300);
  } catch {
    // 浏览器未解锁音频上下文时静默忽略
  }
}

function showBrowserNotification(ev: BrowserNotifyEvent) {
  const isApproval = ev.kind === "approval_pending";
  const navigate = () => {
    if (!ev.url) return;
    window.focus();
    window.location.assign(ev.url);
  };

  if (typeof window !== "undefined" && "Notification" in window && Notification.permission === "granted") {
    try {
      const n = new Notification(ev.title, {
        body: ev.body,
        tag: ev.tag || `vestrail-${ev.seq}`,
        icon: "/icon.png",
        requireInteraction: ev.require_interaction || isApproval,
      });
      n.onclick = () => {
        n.close();
        navigate();
      };
    } catch {
      // 某些受限环境无法构造 Notification 时回退到页内 toast
    }
  }

  if (ev.sound) {
    playChime();
  }

  const toastOpts = {
    description: ev.body,
    duration: isApproval ? 12000 : 6000,
    action: ev.url
      ? {
          label: isApproval ? "去审批" : "查看",
          onClick: navigate,
        }
      : undefined,
  };
  if (isApproval) {
    toast.warning(ev.title, toastOpts);
  } else {
    toast.info(ev.title, toastOpts);
  }
}

export function BrowserNotifyListener() {
  const lastSeqRef = React.useRef<number | undefined>(undefined);

  React.useEffect(() => {
    if (typeof window === "undefined") return;
    const rawSaved = window.sessionStorage.getItem(CURSOR_STORAGE_KEY);
    if (rawSaved !== null) {
      const saved = Number(rawSaved);
      if (Number.isFinite(saved) && saved >= 0) {
        lastSeqRef.current = saved;
      }
    }

    let stopped = false;
    let timer: number | undefined;

    const poll = async () => {
      if (stopped) return;
      let nextDelay = 3000;
      try {
        const res = await api.notifyBrowserEvents(lastSeqRef.current);
        if (stopped) return;
        if (lastSeqRef.current !== undefined && res.enabled && res.items.length > 0) {
          for (const item of res.items) {
            showBrowserNotification(item);
          }
        }
        lastSeqRef.current = res.latest_seq;
        window.sessionStorage.setItem(CURSOR_STORAGE_KEY, String(res.latest_seq));
        if (!res.enabled) {
          nextDelay = 15000;
        }
      } catch {
        nextDelay = 10000;
      }
      if (!stopped) {
        timer = window.setTimeout(() => { void poll(); }, nextDelay);
      }
    };

    void poll();
    const onVisible = () => {
      if (document.visibilityState === "visible" && !stopped) {
        if (timer !== undefined) window.clearTimeout(timer);
        void poll();
      }
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      stopped = true;
      if (timer !== undefined) window.clearTimeout(timer);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, []);

  return null;
}
